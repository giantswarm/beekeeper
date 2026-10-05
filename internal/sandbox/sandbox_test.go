package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
)

// policy is a policy for a home directory in a temporary directory, with a
// checkout configured and a credential beside it.
func policy(t *testing.T) (Policy, string) {
	t.Helper()
	home := testHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	for _, d := range []string{"projects/repo", ".kube", ".claude/projects/p/memory", ".local/state/beekeeper", "go/pkg/mod"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{".kube/config", "projects/repo/go.mod"} {
		if err := os.WriteFile(filepath.Join(home, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Sandbox{AllowRead: []string{filepath.Join(home, "projects")}, AllowWrite: []string{filepath.Join(home, "go/pkg/mod")},
		Domains: []string{"muster.example.com"}, Mask: []config.SandboxMask{{Name: "GH_TOKEN", Hosts: config.GitHubHosts}}}
	return New(cfg, Paths{Home: home, ConfigFile: filepath.Join(home, ".config/beekeeper/config.yaml"),
		StateDir: filepath.Join(home, ".local/state/beekeeper"), LeaseDir: filepath.Join(home, ".local/state/beekeeper/leases"),
		SlotDir: filepath.Join(home, ".local/state/memcap/slots"), Exe: filepath.Join(home, ".go/bin/beekeeper"),
		ScanDir: filepath.Join(home, ".local/state/beekeeper/scan")}), home
}

func TestSettings(t *testing.T) {
	p, _ := policy(t)
	b, err := p.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Env     map[string]string `json:"env"`
		Sandbox struct {
			Enabled, FailIfUnavailable, AllowUnsandboxedCommands bool
			Filesystem                                           struct{ DenyRead, DenyWrite, AllowRead, AllowWrite []string }
			Network                                              struct {
				AllowedDomains          []string
				AllowManagedDomainsOnly bool
				AllowAllUnixSockets     bool
				TLSTerminate            map[string]any
			}
			Credentials struct {
				EnvVars []struct{ Name, Mode string }
			}
		}
		Hooks struct {
			PreToolUse []struct{ Matcher string }
		}
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	sb := s.Sandbox
	if s.Env[Env] != "1" || !sb.Enabled || !sb.FailIfUnavailable || sb.AllowUnsandboxedCommands {
		t.Errorf("the sandbox is not on and closed: %s", b)
	}
	if !slices.Equal(sb.Filesystem.DenyRead, []string{"~/", "~/.local/state/beekeeper/scan"}) {
		t.Errorf("denyRead = %v, want the home directory and the scanner's", sb.Filesystem.DenyRead)
	}
	if !slices.Equal(sb.Filesystem.DenyWrite, []string{"~/.local/state/beekeeper/scan"}) {
		t.Errorf("denyWrite = %v, want the scanner's directory", sb.Filesystem.DenyWrite)
	}
	for _, want := range []string{"~/projects", "~/.go/bin", "~/.config/beekeeper", "~/.claude/projects"} {
		if !slices.Contains(sb.Filesystem.AllowRead, want) {
			t.Errorf("allowRead %v lacks %s", sb.Filesystem.AllowRead, want)
		}
	}
	for _, deny := range []string{"~/.kube", "~/.claude", "~/.config/gh", "~/.ssh"} {
		if slices.Contains(sb.Filesystem.AllowRead, deny) {
			t.Errorf("allowRead re-allows %s", deny)
		}
	}
	if !slices.Contains(sb.Filesystem.AllowWrite, "~/go/pkg/mod") || !slices.Contains(sb.Filesystem.AllowWrite, "~/.local/state/beekeeper") ||
		!slices.Contains(sb.Filesystem.AllowWrite, "~/.local/state/memcap/slots") {
		t.Errorf("allowWrite = %v", sb.Filesystem.AllowWrite)
	}
	if !sb.Network.AllowManagedDomainsOnly || sb.Network.AllowAllUnixSockets || sb.Network.TLSTerminate == nil || !slices.Contains(sb.Network.AllowedDomains, "muster.example.com") ||
		!slices.Contains(sb.Network.AllowedDomains, "api.github.com") && !slices.Contains(sb.Network.AllowedDomains, "*.github.com") {
		t.Errorf("network = %+v", sb.Network)
	}
	if len(sb.Credentials.EnvVars) != 1 || sb.Credentials.EnvVars[0].Name != "GH_TOKEN" || sb.Credentials.EnvVars[0].Mode != "mask" {
		t.Errorf("credentials = %+v", sb.Credentials)
	}
	if len(s.Hooks.PreToolUse) != 1 || s.Hooks.PreToolUse[0].Matcher != "Read|Grep|Glob|Edit|MultiEdit|Write|NotebookEdit" {
		t.Errorf("hooks = %+v", s.Hooks)
	}
}

func TestSettingPrefixes(t *testing.T) {
	p := Policy{Home: "/home/u"}
	for in, want := range map[string]string{"/home/u": "~/", "/home/u/projects": "~/projects", "/var/tmp/x": "//var/tmp/x", "/home/user2": "//home/user2"} {
		if got := p.setting(in); got != want {
			t.Errorf("setting(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestReadable(t *testing.T) {
	p, home := policy(t)
	cwd := filepath.Join(home, "projects/repo")
	for path, want := range map[string]bool{
		filepath.Join(home, ".kube/config"):         false,
		filepath.Join(home, ".ssh/id_ed25519"):      false, // missing, still under the denied home
		home:                                        false,
		filepath.Join(home, "projects/repo/go.mod"): true,
		"go.mod":             true, // relative to cwd
		"../../.kube/config": false,
		filepath.Join(home, ".claude/projects/p/x.jsonl"):     true,
		filepath.Join(home, ".claude/.credentials.json"):      false,
		filepath.Join(home, ".local/state/beekeeper/x.json"):  true,
		filepath.Join(home, "go/pkg/mod/cache"):               true,
		"/etc/hosts":                                          true,
		filepath.Join(home, ".config/beekeeper/config.yaml"):  true,
		filepath.Join(home, ".config/beekeeper2/secret.yaml"): false,
	} {
		if got := p.Readable(path, cwd); got != want {
			t.Errorf("Readable(%s) = %v, want %v", path, got, want)
		}
	}
}

func TestReadableFollowsSymlinks(t *testing.T) {
	p, home := policy(t)
	cwd := filepath.Join(home, "projects/repo")
	link := filepath.Join(cwd, "kube")
	if err := os.Symlink(filepath.Join(home, ".kube"), link); err != nil {
		t.Fatal(err)
	}
	if p.Readable(filepath.Join(link, "config"), cwd) {
		t.Error("a symlink in the checkout re-opened the denied ~/.kube")
	}
	if p.Writable(filepath.Join(link, "new"), cwd) {
		t.Error("a symlink in the checkout made ~/.kube writable")
	}
}

func TestWritable(t *testing.T) {
	p, home := policy(t)
	cwd := filepath.Join(home, "projects/repo")
	for path, want := range map[string]bool{
		filepath.Join(cwd, "new/dir/file.go"):                     true,
		filepath.Join(home, "projects/other/x"):                   false, // readable, not writable
		filepath.Join(home, "x.txt"):                              false,
		filepath.Join(home, ".claude/projects/p/memory/m.md"):     true,
		filepath.Join(home, ".claude/projects/p/x.jsonl"):         false,
		filepath.Join(home, ".claude/settings.json"):              false,
		filepath.Join(os.TempDir(), "probe"):                      true,
		filepath.Join(home, ".local/state/beekeeper/state.json"):  true,
		filepath.Join(home, ".local/state/beekeeper2/state.json"): false,
		filepath.Join(home, ".local/state/beekeeper/scan/key"):    false,
		filepath.Join(home, ".local/state/beekeeper/scan"):        false,
	} {
		if got := p.Writable(path, cwd); got != want {
			t.Errorf("Writable(%s) = %v, want %v", path, got, want)
		}
	}
}

// testHome is a home directory outside the temporary directory, which the
// sandbox keeps writable.
func testHome(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(base, "tmp"))
	return filepath.Join(base, "home")
}

func TestHomeAsWorkingDirectoryOpensNothing(t *testing.T) {
	p, home := policy(t)
	for _, path := range []string{filepath.Join(home, "x.txt"), filepath.Join(home, ".kube/config")} {
		if p.Readable(path, home) || p.Writable(path, home) {
			t.Errorf("with the home directory as cwd, %s is open", path)
		}
	}
}

func TestTheScannersDirectoryIsDeniedInsideTheState(t *testing.T) {
	p, home := policy(t)
	state := filepath.Join(home, ".local/state/beekeeper")
	for _, path := range []string{filepath.Join(state, "scan"), filepath.Join(state, "scan/key"), filepath.Join(state, "scan/index.json")} {
		if p.Readable(path, state) || p.Writable(path, state) {
			t.Errorf("%s is open to the sandbox", path)
		}
	}
	if !p.Readable(filepath.Join(state, "state.json"), state) || !p.Writable(filepath.Join(state, "scanned.json"), state) {
		t.Error("the rest of the state directory is closed")
	}
}
