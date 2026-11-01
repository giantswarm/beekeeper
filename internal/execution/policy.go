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

// The codex adapter relies on `exec --json` thread events, `exec resume`,
// PreToolUse hooks from CODEX_HOME and --dangerously-bypass-hook-trust.
const CodexVersions = ">= 0.165.0, < 1.0.0"

// The omp adapter relies on print mode's JSON events (session, turn_start),
// --resume of a session in --session-dir, and blocking tool_call handlers of
// an explicit --hook extension while discovery is off (--no-extensions).
const OmpVersions = ">= 18.13.0, < 19.0.0"

func (c HostConfig) Validate() error {
	if !ValidIdentity(string(c.Identity)) || !HostPath(c.WorkspaceRoot) || c.WorkspaceRoot == "/" || !HostPath(c.RulesFile) || c.MaxBusy < 1 || len(c.Repositories) == 0 || len(c.Coordinators) == 0 || len(c.Harnesses) == 0 {
		return errors.New("execution host requires identity, workspace root, repositories, coordinators, maxBusy and harness claude, codex or omp")
	}
	for repo, p := range c.Repositories {
		if !repoPattern.MatchString(repo) || !HostPath(p) {
			return errors.New("invalid execution host repository mapping")
		}
		if within(c.WorkspaceRoot, p) || within(p, c.WorkspaceRoot) {
			return errors.New("workspace root and source checkouts must be disjoint")
		}
	}
	for name, h := range c.Harnesses {
		if err := h.validate(name); err != nil {
			return err
		}
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

func (h HarnessConfig) validate(name string) error {
	constraint := map[string]string{HarnessClaude: ClaudeVersions, HarnessCodex: CodexVersions, HarnessOmp: OmpVersions}[name]
	if constraint == "" {
		return errors.New("execution host harnesses must be claude, codex or omp")
	}
	v, err := semver.StrictNewVersion(h.Version)
	rangeOK, _ := semver.NewConstraint(constraint)
	if !HostPath(h.Executable) || err != nil || !rangeOK.Check(v) || h.ConfigDir != "" && !HostPath(h.ConfigDir) {
		return errors.New(name + " requires an absolute executable and a pinned version in " + constraint)
	}
	// Codex keeps its login in the person's own CODEX_HOME on the host; the
	// managed home links only that file and never receives a copied credential.
	if name == HarnessCodex && h.ConfigDir == "" {
		return errors.New("codex requires configDir, the host CODEX_HOME holding its own login")
	}
	// omp reads its models and the provider key from the host's own agent
	// directory; the coordinator never hands it a key.
	if name == HarnessOmp && h.ConfigDir == "" {
		return errors.New("omp requires configDir, the host omp agent directory holding its own models and provider key")
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
	// CodexDigest binds the managed CODEX_HOME's config, hooks and rules. It
	// is empty on a host that runs no codex, leaving its policy unchanged.
	CodexDigest string `json:"codexDigest,omitempty"`
	// OmpDigest binds the managed omp guard extension and rules. It is empty
	// on a host that runs no omp.
	OmpDigest string `json:"ompDigest,omitempty"`
}

func (p Policy) Digest() string { b, _ := json.Marshal(p); return Digest(b) }

// Policy describes precisely what prepare installs. ConfigurationDigest also
// binds the caller's host-local guard configuration, not just these fields.
func (s *Server) Policy() Policy {
	b, _ := json.Marshal(struct {
		Host     HostConfig
		Security string
	}{s.Config, s.SecurityDigest})
	p := Policy{Protocol: Protocol, Version: s.Version, ConfigurationDigest: Digest(b), RulesDigest: Digest(s.Rules), SettingsDigest: Digest(s.settings())}
	if _, ok := s.Config.Harnesses[HarnessCodex]; ok {
		p.CodexDigest = Digest(mustJSON(s.codexFiles()))
	}
	if _, ok := s.Config.Harnesses[HarnessOmp]; ok {
		p.OmpDigest = Digest(mustJSON(s.ompFiles()))
	}
	return p
}

// codexHomeDir is the managed CODEX_HOME of every remote codex worker on
// this host: policy-owned config, hooks and rules, never the person's own.
func (s *Server) codexHomeDir() string { return filepath.Join(s.Root, "codex-home") }

// codexConfig is the managed home's config.toml, the local worker's settings
// without any person's project trust: the hook surface is the only extension,
// and browser, computer, code-mode, image and plugin tools are off.
const codexConfig = `project_doc_max_bytes = 262144

[shell_environment_policy]
inherit = "all"

[features]
hooks = true
daemon_auto_start = false
plugins = false
apps = false
multi_agent = false
multi_agent_v2 = false
unified_exec_tty = false
browser_use = false
browser_use_external = false
browser_use_full_cdp_access = false
computer_use = false
code_mode_host = false
image_generation = false
remote_plugin = false
skill_mcp_dependency_install = false
`

// codexFiles are the policy-bound files of the managed CODEX_HOME. Hooks
// enabled, every plugin, app and sub-agent surface disabled; the hook is the
// execution guard in its codex format, bound to this host's configuration.
func (s *Server) codexFiles() map[string][]byte {
	cmd := quote(s.Executable) + " --config " + quote(s.ConfigPath) + " agents execution-guard --format codex"
	hooks := mustJSON(map[string]any{hooksKey: preToolUse(cmd)})
	return map[string][]byte{"config.toml": []byte(codexConfig), "hooks.json": hooks, "AGENTS.md": s.Rules}
}

// linkCodexLogin links the person's own host login into the managed home. A
// refreshed login codex already rewrote in place is left as it is.
func (s *Server) linkCodexLogin() error {
	h, ok := s.Config.Harnesses[HarnessCodex]
	if !ok {
		return nil
	}
	link := filepath.Join(s.codexHomeDir(), "auth.json")
	if _, err := os.Lstat(link); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Symlink(filepath.Join(h.ConfigDir, "auth.json"), link)
}

const hooksKey = "hooks"

// preToolUse is the hook table both harnesses read: one command for every tool.
func preToolUse(command string) map[string]any {
	const kind = "command"
	return map[string]any{"PreToolUse": []any{map[string]any{"matcher": ".*", hooksKey: []any{map[string]any{"type": kind, kind: command, "timeout": 30}}}}}
}

func (s *Server) settings() []byte {
	cmd := quote(s.Executable) + " --config " + quote(s.ConfigPath) + " agents execution-guard"
	b, _ := json.Marshal(map[string]any{
		hooksKey:      preToolUse(cmd),
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
	for name, b := range map[string][]byte{"settings.json": s.settings(), "CLAUDE.md": s.Rules} {
		if err := atomicFile(filepath.Join(s.Root, name), b); err != nil {
			return "", err
		}
	}
	if p.CodexDigest != "" {
		if err := ownedDirectory(s.codexHomeDir()); err != nil {
			return "", err
		}
		for name, b := range s.codexFiles() {
			if err := atomicFile(filepath.Join(s.codexHomeDir(), name), b); err != nil {
				return "", err
			}
		}
		if err := s.linkCodexLogin(); err != nil {
			return "", err
		}
	}
	if p.OmpDigest != "" {
		for _, dir := range []string{s.ompDir(), s.ompSessionsDir()} {
			if err := ownedDirectory(dir); err != nil {
				return "", err
			}
		}
		for name, b := range s.ompFiles() {
			if err := atomicFile(filepath.Join(s.ompDir(), name), b); err != nil {
				return "", err
			}
		}
	}
	// The manifest comes last: a host is prepared only once every file is.
	if err := atomicFile(filepath.Join(s.Root, "policy.json"), mustJSON(p)); err != nil {
		return "", err
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
	if p.CodexDigest != "" {
		for name, want := range s.codexFiles() {
			b, err := readRegular(filepath.Join(s.codexHomeDir(), name), MaxWire)
			if err != nil || Digest(b) != Digest(want) {
				return p, errors.New("installed codex rules, configuration or hooks do not match the policy")
			}
		}
	}
	if p.OmpDigest != "" {
		for name, want := range s.ompFiles() {
			b, err := readRegular(filepath.Join(s.ompDir(), name), MaxWire)
			if err != nil || Digest(b) != Digest(want) {
				return p, errors.New("installed omp rules or guard extension do not match the policy")
			}
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
	bound := filepath.Clean(s.Executable)
	resolved := filepath.Join(filepath.Clean(filepath.Dir(bound)), managedBinary)
	fi, err := os.Stat(resolved)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return errors.New("no executable beekeeper at the bound execution host installation; install it and prepare the matching bundle")
	}
	// The runner prepends this directory and drops its duplicates, so the
	// resolved command is what the bare worker commands reach: it must be
	// the very file the prepared policy binds, not a stale sibling beside
	// a differently named or versioned installation.
	own, err := os.Stat(bound)
	if err != nil || !os.SameFile(fi, own) {
		return errors.New("the bare worker command does not resolve to the bound execution host executable; install the prepared executable as " + managedBinary + " in its directory")
	}
	return nil
}

// Probe runs no model call and returns no authentication values. Its output is
// only metadata. No provider credential from the SSH login shell is inherited.
func (s *Server) probe(ctx context.Context, name string, h HarnessConfig) error {
	switch name {
	case HarnessCodex:
		return s.probeCodex(ctx, h)
	case HarnessOmp:
		return s.probeOmp(ctx, h)
	}
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

// codexEnvironment runs codex against the managed CODEX_HOME only.
func (s *Server) codexEnvironment() []string {
	return append(cleanEnvironment("", s.Executable), "CODEX_HOME="+s.codexHomeDir())
}

// probeCodex checks the pinned version and a ChatGPT subscription login in
// the managed home. `login status` reports on stderr; only its first words
// are compared, never printed: an API-key line can carry a masked key.
func (s *Server) probeCodex(ctx context.Context, h HarnessConfig) error {
	if err := s.linkCodexLogin(); err != nil {
		return errors.New("codex login link in the managed home is unavailable")
	}
	version, err := s.command(ctx, "", s.codexEnvironment(), h.Executable, "--version")
	parts := strings.Fields(string(version))
	if err != nil || len(parts) != 2 || parts[0] != "codex-cli" || parts[1] != h.Version {
		return errors.New("unsupported or mismatched codex version")
	}
	status, err := s.combined(ctx, "", s.codexEnvironment(), h.Executable, "login", "status")
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(status)), "Logged in using ChatGPT") {
		return errors.New("execution host needs an independently provisioned Codex ChatGPT login: run `CODEX_HOME=<configDir> codex login --device-auth` on the host")
	}
	return nil
}
