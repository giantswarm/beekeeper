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
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/giantswarm/beekeeper/internal/agyhooks"
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
	RulesFile     string            `yaml:"rulesFile" json:"rulesFile"`
	Identity      HostID            `yaml:"identity" json:"identity"`
	WorkspaceRoot string            `yaml:"workspaceRoot" json:"workspaceRoot"`
	Repositories  map[string]string `yaml:"repositories" json:"repositories"`
	// RepositoryOrgs admits coordinator-bundled repositories without a source mapping.
	RepositoryOrgs []string                 `yaml:"repositoryOrgs,omitempty" json:"repositoryOrgs,omitempty"`
	Harnesses      map[string]HarnessConfig `yaml:"harnesses" json:"harnesses"`
	Coordinators   map[string]Coordinator   `yaml:"coordinators" json:"coordinators"`
	MaxBusy        int                      `yaml:"maxBusy" json:"maxBusy"`
	// Tools pins the worker toolchain (go, gh) a worker calls by bare name.
	// Omitted, it leaves the configuration digest of a host without it as is.
	Tools map[string]ToolConfig `yaml:"tools,omitempty" json:"tools,omitempty"`
	// Retention is how long a retired execution's moved-aside resources are
	// kept, a Go duration. Omitted, it is DefaultRetention and leaves the
	// configuration digest of a host without it as is.
	Retention string `yaml:"retention,omitempty" json:"retention,omitempty"`
}

// retention is the validated Retention, DefaultRetention when unset.
func (c HostConfig) retention() time.Duration {
	if d, err := time.ParseDuration(c.Retention); err == nil && d > 0 {
		return d
	}
	return DefaultRetention
}

// The adapter relies on restricted settings and auth status's configDirectory
// contract. Pin an exact installed version inside this supported CLI family.
const ClaudeVersions = ">= 2.1.268, < 3.0.0"

// The codex adapter relies on `exec --json` thread events, `exec resume`,
// PreToolUse hooks from CODEX_HOME and --dangerously-bypass-hook-trust.
const CodexVersions = ">= 0.165.0, < 1.0.0"

// The agy adapter relies on `--input-format stream-json --output-format
// stream-json`, `--conversation` resume, and PreToolUse hooks from
// `~/.gemini/config/hooks.json` under the project name key (agy >= 1.4.7).
const AgyVersions = ">= 1.4.7, < 2.0.0"

func (c HostConfig) Validate() error {
	if !ValidIdentity(string(c.Identity)) || !HostPath(c.WorkspaceRoot) || c.WorkspaceRoot == "/" || !HostPath(c.RulesFile) || c.MaxBusy < 1 || len(c.Repositories) == 0 && len(c.RepositoryOrgs) == 0 || len(c.Coordinators) == 0 || len(c.Harnesses) == 0 {
		return errors.New("execution host requires identity, workspace root, repositories, coordinators, maxBusy and harness claude, codex, agy or omp")
	}
	if c.Retention != "" {
		if d, err := time.ParseDuration(c.Retention); err != nil || d <= 0 {
			return errors.New("execution host retention must be a positive duration")
		}
	}
	if err := validateRepositoryOrgs(c.RepositoryOrgs); err != nil {
		return err
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
	for name, t := range c.Tools {
		if err := t.validate(name); err != nil {
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
	constraint := map[string]string{HarnessClaude: ClaudeVersions, HarnessCodex: CodexVersions, HarnessAgy: AgyVersions, HarnessOmp: OmpVersions}[name]
	if constraint == "" {
		return errors.New("execution host harnesses must be claude, codex, agy or omp")
	}
	v, err := semver.StrictNewVersion(h.Version)
	rangeOK, _ := semver.NewConstraint(constraint)
	if !HostPath(h.Executable) || err != nil || !rangeOK.Check(v) || h.ConfigDir != "" && !HostPath(h.ConfigDir) {
		return errors.New(name + " requires an absolute executable and a pinned version in " + constraint)
	}
	// Codex keeps its login in the person's own CODEX_HOME on the host; the
	// managed home links only that file and never receives a copied credential.
	if (name == HarnessCodex || name == HarnessOmp) && h.ConfigDir == "" {
		return errors.New(name + " requires configDir holding its independently provisioned host login")
	}
	// Agy keeps its OAuth login in the host's own agy configuration directory;
	// the managed home links only the token file and never copies credentials.
	if name == HarnessAgy && h.ConfigDir == "" {
		return errors.New("agy requires configDir, the host directory holding its own login")
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
	CoordinatorDigest   string `json:"coordinatorDigest,omitempty"`
	Protocol            int    `json:"protocol"`
	Version             string `json:"version"`
	ConfigurationDigest string `json:"configurationDigest"`
	RulesDigest         string `json:"rulesDigest"`
	SettingsDigest      string `json:"settingsDigest"`
	// CodexDigest binds the managed CODEX_HOME's config, hooks and rules. It
	// is empty on a host that runs no codex, leaving its policy unchanged.
	CodexDigest string `json:"codexDigest,omitempty"`
	OmpDigest   string `json:"ompDigest,omitempty"`
	// AgyDigest binds the managed agy home's config, hooks and rules. It is
	// empty on a host that runs no agy, leaving its policy unchanged.
	AgyDigest string `json:"agyDigest,omitempty"`
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
	if s.coordinatorBundle != nil {
		p.CoordinatorDigest = s.coordinatorBundle.Digest()
	}
	if _, ok := s.Config.Harnesses[HarnessCodex]; ok {
		p.CodexDigest = Digest(mustJSON(s.codexFiles()))
	}
	if _, ok := s.Config.Harnesses[HarnessOmp]; ok {
		p.OmpDigest = Digest(s.ompGuardBytes())
	}
	if _, ok := s.Config.Harnesses[HarnessAgy]; ok {
		p.AgyDigest = Digest(mustJSON(s.agyFiles()))
	}
	return p
}

// codexHomeDir is the managed CODEX_HOME of every remote codex worker on
// this host: policy-owned config, hooks and rules, never the person's own.
func (s *Server) codexHomeDir() string {
	if s.coordinatorBundle != nil {
		return filepath.Join(s.Root, "codex-home", s.coordinatorBundle.Digest())
	}
	return filepath.Join(s.Root, "codex-home")
}

// codexConfig is the managed home's config.toml, the local worker's settings
// without any person's project trust. Code-mode provides the worker's tools;
// hooks guard execution, while browser, computer, image and plugin tools are off.
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
code_mode_host = true
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
	files := map[string][]byte{"config.toml": []byte(codexConfig), hooksJSONFile: hooks, "AGENTS.md": append([]byte(nil), s.Rules...)}
	if b := s.coordinatorBundle; b != nil {
		for _, a := range b.Artifacts {
			if strings.HasPrefix(a.Artifact, "rules/") {
				files["AGENTS.md"] = append(files["AGENTS.md"], append([]byte("\n\nCoordinator rules:\n"), a.Content...)...)
			} else {
				files[a.Artifact] = a.Content
			}
		}
		if value, ok := b.Settings["model_reasoning_effort"]; ok {
			files["config.toml"] = []byte("model_reasoning_effort = \"" + value + "\"\n" + codexConfig)
		}
	}
	return files
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

const (
	agyLoginToken  = "antigravity-oauth-token" //nolint:gosec // OAuth token file basename, not a credential value
	hooksJSONFile  = "hooks.json"
	mcpServersKey  = "mcpServers"
	streamJSONMode = "stream-json"
)

// agyHomeDir is the managed HOME of every remote agy worker on this host:
// policy-owned config, hooks and rules under .gemini, never the person's own.
func (s *Server) agyHomeDir() string { return filepath.Join(s.Root, "agy-home") }

func (s *Server) agyConfigDir() string {
	return filepath.Join(s.agyHomeDir(), ".gemini", "config")
}

func (s *Server) agyAppDir() string {
	return filepath.Join(s.agyHomeDir(), ".gemini", "antigravity-cli")
}

// agyFiles are the policy-bound files of the managed agy home (under
// .gemini/config). Hooks are enabled under the project name in the shape agy
// >= 1.4.7 requires, user MCP servers and skills are cleared, and embedded
// worker rules are installed as GEMINI.md.
func (s *Server) agyFiles() map[string][]byte {
	cmd := quote(s.Executable) + " --config " + quote(s.ConfigPath) + " agents execution-guard --format agy"
	hooks := mustJSON(map[string]any{managedBinary: agyPreToolUse(cmd)})
	mcp := mustJSON(map[string]any{mcpServersKey: map[string]any{}})
	skills := mustJSON(map[string]any{"entries": []any{}})
	return map[string][]byte{"GEMINI.md": s.Rules, hooksJSONFile: hooks, "mcp_config.json": mcp, "skills.json": skills}
}

func agyPreToolUse(command string) map[string]any {
	return preToolUseMatcher("*", command)
}

func agyLoginSource(configDir string) string {
	for _, candidate := range []string{
		filepath.Join(configDir, ".gemini", "antigravity-cli", agyLoginToken),
		filepath.Join(configDir, "antigravity-cli", agyLoginToken),
		filepath.Join(configDir, agyLoginToken),
	} {
		if _, err := os.Lstat(candidate); err == nil {
			return candidate
		}
	}
	return filepath.Join(configDir, agyLoginToken)
}

// linkAgyLogin links the person's own host OAuth token into the managed agy
// home. A refreshed token rewritten in place is left as it is.
func (s *Server) linkAgyLogin() error {
	h, ok := s.Config.Harnesses[HarnessAgy]
	if !ok {
		return nil
	}
	if err := ownedDirectory(s.agyHomeDir()); err != nil {
		return err
	}
	if err := ownedDirectory(filepath.Join(s.agyHomeDir(), ".gemini")); err != nil {
		return err
	}
	if err := ownedDirectory(s.agyAppDir()); err != nil {
		return err
	}
	link := filepath.Join(s.agyAppDir(), agyLoginToken)
	if _, err := os.Stat(link); err == nil {
		return nil
	}
	src := agyLoginSource(h.ConfigDir)
	if target, err := os.Readlink(link); err == nil {
		if target == src {
			return nil
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Symlink(src, link)
}

const hooksKey = "hooks"

// preToolUse is the hook table both harnesses read: one command for every tool.
func preToolUse(command string) map[string]any {
	return preToolUseMatcher(".*", command)
}

func preToolUseMatcher(matcher, command string) map[string]any {
	const kind = "command"
	return map[string]any{"PreToolUse": []any{map[string]any{"matcher": matcher, hooksKey: []any{map[string]any{"type": kind, kind: command, "timeout": 30}}}}}
}

func (s *Server) settings() []byte {
	cmd := quote(s.Executable) + " --config " + quote(s.ConfigPath) + " agents execution-guard"
	b, _ := json.Marshal(map[string]any{
		hooksKey:      preToolUse(cmd),
		"permissions": map[string]any{"deny": []string{"SendMessage", "AskUserQuestion", "Task", "Agent"}},
	})
	return b
}

func (s *Server) Prepare(p Policy) (string, error) {
	if err := s.Config.Validate(); err != nil {
		return "", err
	}
	if s.coordinatorBundle != nil {
		if err := s.coordinatorBundle.Validate(); err != nil {
			return "", err
		}
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
	for name, b := range map[string][]byte{claudeSettingsFile: s.settings(), claudeRulesFile: s.Rules} {
		if err := atomicFile(filepath.Join(s.Root, name), b); err != nil {
			return "", err
		}
	}
	if p.CodexDigest != "" {
		if err := ownedDirectory(s.codexHomeDir()); err != nil {
			return "", err
		}
		for name, b := range s.codexFiles() {
			if err := ownedDirectory(filepath.Dir(filepath.Join(s.codexHomeDir(), name))); err != nil {
				return "", err
			}
			if err := atomicFile(filepath.Join(s.codexHomeDir(), name), b); err != nil {
				return "", err
			}
		}
		if err := s.linkCodexLogin(); err != nil {
			return "", err
		}
	}
	if p.OmpDigest != "" {
		if err := atomicFile(s.ompGuardFile(), s.ompGuardBytes()); err != nil {
			return "", err
		}
	}
	if p.AgyDigest != "" {
		if err := ownedDirectory(s.agyHomeDir()); err != nil {
			return "", err
		}
		if err := ownedDirectory(filepath.Join(s.agyHomeDir(), ".gemini")); err != nil {
			return "", err
		}
		if err := ownedDirectory(s.agyConfigDir()); err != nil {
			return "", err
		}
		if err := ownedDirectory(s.agyAppDir()); err != nil {
			return "", err
		}
		for name, b := range s.agyFiles() {
			if err := atomicFile(filepath.Join(s.agyConfigDir(), name), b); err != nil {
				return "", err
			}
		}
		if err := s.linkAgyLogin(); err != nil {
			return "", err
		}
	}
	if s.coordinatorBundle != nil {
		if err := atomicFile(filepath.Join(s.Root, "coordinator-bundle.json"), mustJSON(s.coordinatorBundle)); err != nil {
			return "", err
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
	if s.coordinatorBundle != nil {
		data, err := readRegular(filepath.Join(s.Root, "coordinator-bundle.json"), maxBundleBytes)
		if err != nil || Digest(data) != Digest(mustJSON(s.coordinatorBundle)) {
			return p, errors.New("installed coordinator bundle does not match policy")
		}
	}
	for name, digest := range map[string]string{claudeSettingsFile: p.SettingsDigest, claudeRulesFile: p.RulesDigest} {
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
		b, err := readRegular(s.ompGuardFile(), MaxWire)
		if err != nil || Digest(b) != p.OmpDigest {
			return p, errors.New("installed omp guard does not match the policy")
		}
	}
	if p.AgyDigest != "" {
		for name, want := range s.agyFiles() {
			b, err := readRegular(filepath.Join(s.agyConfigDir(), name), MaxWire)
			if err != nil || Digest(b) != Digest(want) {
				return p, errors.New("installed agy rules, configuration or hooks do not match the policy")
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
func (s *Server) probe(ctx context.Context, name string, h HarnessConfig, readiness *HarnessReadiness) error {
	switch name {
	case HarnessCodex:
		return s.probeCodex(ctx, h, readiness)
	case HarnessAgy:
		return s.probeAgy(ctx, h, readiness)
	}
	if name == HarnessOmp {
		return s.probeOmp(ctx, h, readiness)
	}
	version, err := s.command(ctx, "", cleanEnvironment(h.ConfigDir, s.Executable), h.Executable, "--restricted", "--settings", filepath.Join(s.Root, claudeSettingsFile), "--version")
	parts := strings.Fields(string(version))
	if err != nil || len(parts) == 0 || parts[0] != h.Version {
		readiness.VersionCheck = probeFailed
		return errors.New("unsupported or mismatched harness version/flags")
	}
	readiness.VersionCheck, readiness.Login = probePassed, probeFailed
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
	readiness.Login = probePassed
	return nil
}

// codexEnvironment runs codex against the managed CODEX_HOME only.
func (s *Server) codexEnvironment() []string {
	return append(cleanEnvironment("", s.Executable), "CODEX_HOME="+s.codexHomeDir())
}

// probeCodex checks the pinned version and a ChatGPT subscription login in
// the managed home. `login status` reports on stderr; only its first words
// are compared, never printed: an API-key line can carry a masked key.
func (s *Server) probeCodex(ctx context.Context, h HarnessConfig, readiness *HarnessReadiness) error {
	if err := s.linkCodexLogin(); err != nil {
		readiness.Login = probeFailed
		return errors.New("codex login link in the managed home is unavailable")
	}
	version, err := s.command(ctx, "", s.codexEnvironment(), h.Executable, "--version")
	parts := strings.Fields(string(version))
	if err != nil || len(parts) != 2 || parts[0] != "codex-cli" || parts[1] != h.Version {
		readiness.VersionCheck = probeFailed
		return errors.New("unsupported or mismatched codex version")
	}
	readiness.VersionCheck, readiness.Login = probePassed, probeFailed
	status, err := s.combined(ctx, "", s.codexEnvironment(), h.Executable, "login", "status")
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(status)), "Logged in using ChatGPT") {
		return errors.New("execution host needs an independently provisioned Codex ChatGPT login: run `CODEX_HOME=<configDir> codex login --device-auth` on the host")
	}
	readiness.Login = probePassed
	return nil
}

// agyEnvironment runs agy against the managed agy home only while keeping the
// host account's Go build and module caches for its probes; a worker's run
// replaces them with its owned paths (ownedEnvironment).
func (s *Server) agyEnvironment() []string {
	return s.agyEnvironmentWithHome(s.agyHomeDir())
}

func (s *Server) agyEnvironmentWithHome(home string) []string {
	var out []string
	userHome := os.Getenv(EnvHome)
	if userHome == "" {
		userHome = os.Getenv("HOME")
	}
	for _, kv := range cleanEnvironment("", s.Executable) {
		if strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, EnvHome+"=") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "HOME="+home)
	if userHome != "" {
		out = append(out, EnvHome+"="+userHome)
		if v := os.Getenv("GOPATH"); v != "" {
			out = append(out, "GOPATH="+v)
		} else {
			out = append(out, "GOPATH="+filepath.Join(userHome, "go"))
		}
		if v := os.Getenv("GOCACHE"); v != "" {
			out = append(out, "GOCACHE="+v)
		} else {
			out = append(out, "GOCACHE="+filepath.Join(userHome, ".cache", "go-build"))
		}
	}
	return out
}

// probeAgy checks the pinned version and an active agy OAuth login in the
// managed home without making a model generation call.
func (s *Server) probeAgy(ctx context.Context, h HarnessConfig, readiness *HarnessReadiness) error {
	if err := s.linkAgyLogin(); err != nil {
		readiness.Login = probeFailed
		return errors.New("agy login link in the managed home is unavailable")
	}
	if err := s.agyHooksLoad(); err != nil {
		readiness.Guard = probeFailed
		return err
	}
	version, err := s.command(ctx, "", s.agyEnvironment(), h.Executable, "--version")
	parts := strings.Fields(string(version))
	gotVer := ""
	switch len(parts) {
	case 1:
		gotVer = parts[0]
	case 2:
		if parts[0] == HarnessAgy {
			gotVer = parts[1]
		}
	case 3:
		if parts[0] == HarnessAgy && parts[1] == "version" {
			gotVer = parts[2]
		}
	}
	if err != nil || gotVer != h.Version {
		readiness.VersionCheck = probeFailed
		return errors.New("unsupported or mismatched agy version")
	}
	readiness.VersionCheck, readiness.Login = probePassed, probeFailed
	modelsOut, err := s.combined(ctx, "", s.agyEnvironment(), h.Executable, "models")
	if err != nil || !agyHasModels(string(modelsOut)) {
		return errors.New("execution host needs an independently provisioned agy login: run `HOME=<configDir> agy` on the host")
	}
	readiness.Login = probePassed
	return nil
}

// agyHooksLoad fails when agy would reject the managed hooks.json: agy drops
// the whole file for one entry it rejects, and its workers would run with
// no execution guard at all.
func (s *Server) agyHooksLoad() error {
	b, err := readRegular(filepath.Join(s.agyConfigDir(), hooksJSONFile), MaxWire)
	if err != nil {
		return errors.New("the managed agy hooks.json is unreadable; prepare the matching bundle")
	}
	if why := agyhooks.Rejects(b); why != "" {
		return errors.New("agy would reject the managed hooks.json and run unguarded: " + why)
	}
	return nil
}

func agyHasModels(raw string) bool {
	for _, line := range strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' }) {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 || strings.EqualFold(fields[0], "fetching") || strings.EqualFold(fields[0], "not") {
			continue
		}
		return true
	}
	return false
}
