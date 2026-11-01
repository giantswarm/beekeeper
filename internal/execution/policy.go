package execution

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver/v3"
)

type HarnessConfig struct {
	Executable string `yaml:"executable" json:"executable"`
	Version    string `yaml:"version" json:"version"`
	ConfigDir  string `yaml:"configDir,omitempty" json:"configDir,omitempty"`
}

type Coordinator struct {
	PublicKey string   `yaml:"publicKey" json:"publicKey"`
	Callback  Endpoint `yaml:"callback" json:"callback"`
}

// HostConfig is operator-owned configuration on the execution host. It is
// never replaced by a request, a brief or the coordinator's filesystem paths.
type HostConfig struct {
	RulesFile     string                   `yaml:"rulesFile" json:"rulesFile"`
	Identity      HostID                   `yaml:"identity" json:"identity"`
	WorkspaceRoot string                   `yaml:"workspaceRoot" json:"workspaceRoot"`
	Repositories  map[string]string        `yaml:"repositories" json:"repositories"`
	Harnesses     map[string]HarnessConfig `yaml:"harnesses" json:"harnesses"`
	Coordinators  map[string]Coordinator   `yaml:"coordinators" json:"coordinators"`
	MaxBusy       int                      `yaml:"maxBusy" json:"maxBusy"`
}

// The adapter relies on restricted settings and auth status's configDirectory
// contract. Pin an exact installed version inside this supported CLI family.
const ClaudeVersions = ">= 2.1.268, < 3.0.0"

func (c HostConfig) Validate() error {
	if !ValidIdentity(string(c.Identity)) || !HostPath(c.WorkspaceRoot) || c.WorkspaceRoot == "/" || !HostPath(c.RulesFile) || c.MaxBusy < 1 || len(c.Repositories) == 0 || len(c.Coordinators) == 0 || len(c.Harnesses) != 1 {
		return errors.New("execution host requires identity, workspace root, repositories, coordinators, maxBusy and harness claude")
	}
	for repo, p := range c.Repositories {
		if !repoPattern.MatchString(repo) || !HostPath(p) {
			return errors.New("invalid execution host repository mapping")
		}
		if within(c.WorkspaceRoot, p) || within(p, c.WorkspaceRoot) {
			return errors.New("workspace root and source checkouts must be disjoint")
		}
	}
	h, ok := c.Harnesses[HarnessClaude]
	v, err := semver.StrictNewVersion(h.Version)
	rangeOK, _ := semver.NewConstraint(ClaudeVersions)
	if !ok || !HostPath(h.Executable) || err != nil || !rangeOK.Check(v) || h.ConfigDir != "" && !HostPath(h.ConfigDir) {
		return errors.New("claude requires an absolute executable and a pinned version in " + ClaudeVersions)
	}
	for id, co := range c.Coordinators {
		if !ValidIdentity(id) {
			return errors.New("invalid trusted coordinator identity")
		}
		if _, err := publicKey(co.PublicKey); err != nil {
			return err
		}
		if err := co.Callback.Validate(); err != nil {
			return fmt.Errorf("coordinator callback: %w", err)
		}
	}
	return nil
}

func publicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("invalid coordinator public key")
	}
	return ed25519.PublicKey(b), nil
}

type Policy struct {
	Protocol            int    `json:"protocol"`
	Version             string `json:"version"`
	ConfigurationDigest string `json:"configurationDigest"`
	RulesDigest         string `json:"rulesDigest"`
	SettingsDigest      string `json:"settingsDigest"`
}

func (p Policy) Digest() string { b, _ := json.Marshal(p); return Digest(b) }

// Policy describes precisely what prepare installs. ConfigurationDigest also
// binds the caller's host-local guard configuration, not just these fields.
func (s *Server) Policy() Policy {
	b, _ := json.Marshal(struct {
		Host     HostConfig
		Security string
	}{s.Config, s.SecurityDigest})
	return Policy{Protocol: Protocol, Version: s.Version, ConfigurationDigest: Digest(b), RulesDigest: Digest(s.Rules), SettingsDigest: Digest(s.settings())}
}

func (s *Server) settings() []byte {
	cmd := quote(s.Executable) + " --config " + quote(s.ConfigPath) + " agents execution-guard"
	b, _ := json.Marshal(map[string]any{
		"hooks":       map[string]any{"PreToolUse": []any{map[string]any{"matcher": ".*", "hooks": []any{map[string]any{"type": "command", "command": cmd, "timeout": 30}}}}},
		"permissions": map[string]any{"deny": []string{"SendMessage", "AskUserQuestion", "Task", "Agent", "mcp__*"}},
	})
	return b
}

func (s *Server) Prepare(p Policy) (string, error) {
	if err := s.Config.Validate(); err != nil {
		return "", err
	}
	if p != s.Policy() {
		return "", errors.New("policy bundle differs from this binary, embedded rules or host guard configuration")
	}
	if !filepath.IsAbs(s.Root) || filepath.Clean(s.Root) == string(filepath.Separator) || within(s.Config.WorkspaceRoot, s.Root) || within(s.Root, s.Config.WorkspaceRoot) {
		return "", errors.New("execution policy and workspace roots must be disjoint absolute directories")
	}
	for _, source := range s.Config.Repositories {
		if within(source, s.Root) || within(s.Root, source) {
			return "", errors.New("execution policy and source checkouts must be disjoint")
		}
	}
	if err := ownedDirectory(s.Root); err != nil {
		return "", err
	}
	if err := ownedDirectory(s.Config.WorkspaceRoot); err != nil {
		return "", err
	}
	for name, b := range map[string][]byte{"policy.json": mustJSON(p), "settings.json": s.settings(), "CLAUDE.md": s.Rules} {
		if err := atomicFile(filepath.Join(s.Root, name), b); err != nil {
			return "", err
		}
	}
	return p.Digest(), nil
}

func (s *Server) InstalledPolicy() (Policy, error) {
	if err := s.Config.Validate(); err != nil {
		return Policy{}, err
	}
	var p Policy
	if err := readJSON(filepath.Join(s.Root, "policy.json"), &p); err != nil {
		return p, errors.New("execution policy is not prepared")
	}
	if p != s.Policy() {
		return p, errors.New("installed execution policy is stale; prepare the matching bundle")
	}
	for name, digest := range map[string]string{"settings.json": p.SettingsDigest, "CLAUDE.md": p.RulesDigest} {
		b, err := readRegular(filepath.Join(s.Root, name), MaxWire)
		if err != nil || Digest(b) != digest {
			return p, errors.New("installed execution rules or hooks do not match the policy")
		}
	}
	return p, nil
}

// workerExecutable fails closed when the embedded worker contract cannot
// execute its component. The managed turn commands the host by bare
// `beekeeper` through the harness PATH, which the runner pins to this
// installation's directory, so readiness must refuse a host where note, park
// and report cannot run instead of starting a worker that cannot reach its
// coordinator. A per-user installation is accepted; a system-wide one is not
// required.
func (s *Server) workerExecutable() error {
	install := filepath.Join(filepath.Clean(filepath.Dir(filepath.Clean(s.Executable))), managedBinary)
	fi, err := os.Stat(install)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return errors.New("no executable beekeeper at the bound execution host installation; install it and prepare the matching bundle")
	}
	return nil
}

// Probe runs no model call and returns no authentication values. Its output is
// only metadata. No provider credential from the SSH login shell is inherited.
func (s *Server) probe(ctx context.Context, h HarnessConfig) error {
	version, err := s.command(ctx, "", cleanEnvironment(h.ConfigDir, s.Executable), h.Executable, "--restricted", "--settings", filepath.Join(s.Root, "settings.json"), "--version")
	parts := strings.Fields(string(version))
	if err != nil || len(parts) == 0 || parts[0] != h.Version {
		return errors.New("unsupported or mismatched harness version/flags")
	}
	raw, err := s.command(ctx, "", cleanEnvironment(h.ConfigDir, s.Executable), h.Executable, authCommand, "status")
	if err != nil {
		return errors.New("harness authentication is not ready; provision it on the execution host")
	}
	var auth struct {
		LoggedIn        bool   `json:"loggedIn"`
		ConfigDirectory string `json:"configDirectory"`
		AuthMethod      string `json:"authMethod"`
	}
	if json.Unmarshal(raw, &auth) != nil || !auth.LoggedIn || auth.ConfigDirectory == "" || auth.AuthMethod != "claude.ai" {
		return errors.New("execution host needs an independently provisioned Claude subscription login")
	}
	if h.ConfigDir != "" && filepath.Clean(auth.ConfigDirectory) != filepath.Clean(h.ConfigDir) {
		return errors.New("harness authentication belongs to another configuration directory")
	}
	return nil
}
