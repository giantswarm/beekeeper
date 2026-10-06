package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
		Domains: []string{"muster.example.com"}, ProxyPort: 3190}
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
				HTTPProxyPort           int
				SOCKSProxyPort          int
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
	if !slices.Equal(sb.Filesystem.DenyWrite, []string{"~/.local/state/beekeeper/scan", "~/.claude"}) {
		t.Errorf("denyWrite = %v, want the scanner's directory and the harness's", sb.Filesystem.DenyWrite)
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
	if !sb.Network.AllowManagedDomainsOnly || sb.Network.AllowAllUnixSockets || sb.Network.HTTPProxyPort != 3190 || sb.Network.SOCKSProxyPort != 3190 || !slices.Contains(sb.Network.AllowedDomains, "muster.example.com") ||
		!slices.Contains(sb.Network.AllowedDomains, "api.github.com") && !slices.Contains(sb.Network.AllowedDomains, "*.github.com") {
		t.Errorf("network = %+v", sb.Network)
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
	t.Setenv("XDG_CONFIG_HOME", "")
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

func TestMountPointsCreatesTheDeniedPathsOnAFreshState(t *testing.T) {
	p, home := policy(t)
	scan := filepath.Join(home, ".local/state/beekeeper/scan")
	if _, err := os.Stat(scan); !os.IsNotExist(err) {
		t.Fatalf("the fixture has %s already: %v", scan, err)
	}
	for range 2 {
		if err := p.MountPoints(); err != nil {
			t.Fatal(err)
		}
	}
	if fi, err := os.Stat(scan); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("%s is not a private directory: %v %v", scan, fi, err)
	}
}

func TestNestedRefusesAWritableChildOfAReadableParent(t *testing.T) {
	p, home := policy(t)
	if err := p.Nested(); err != nil {
		t.Fatalf("the fixture's layout: %v", err)
	}
	cfg := filepath.Join(home, ".config")
	q := New(config.Sandbox{AllowRead: []string{cfg}}, Paths{Home: home, ConfigFile: filepath.Join(cfg, "beekeeper/config.yaml"),
		StateDir: filepath.Join(cfg, "beekeeper/state"), Exe: filepath.Join(home, ".go/bin/beekeeper")})
	err := q.Nested()
	if err == nil {
		t.Fatal("a writable state inside a readable config directory is accepted")
	}
	for _, path := range []string{cfg, filepath.Join(cfg, "beekeeper/state")} {
		if !strings.Contains(err.Error(), path) {
			t.Errorf("%q does not name %s", err, path)
		}
	}
}

func TestASymlinkedPathIsListedAtItsTarget(t *testing.T) {
	home := testHome(t)
	dotfiles := filepath.Join(home, "projects/dotfiles/.config/beekeeper")
	if err := os.MkdirAll(dotfiles, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".config/beekeeper")
	if err := os.Symlink(dotfiles, link); err != nil {
		t.Fatal(err)
	}
	p := New(config.Sandbox{}, Paths{Home: home, ConfigFile: filepath.Join(link, "config.yaml"), StateDir: filepath.Join(home, ".local/state/beekeeper")})
	if !slices.Contains(p.Read, link) || !slices.Contains(p.Read, dotfiles) {
		t.Errorf("read %v lacks the link or its target", p.Read)
	}
	if err := os.WriteFile(filepath.Join(dotfiles, "config.yaml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p = New(config.Sandbox{}, Paths{Home: home, ConfigFile: filepath.Join(link, "config.yaml"), StateDir: filepath.Join(home, ".local/state/beekeeper")})
	if got := p.Vars["BEEKEEPER_CONFIG"]; got != filepath.Join(dotfiles, "config.yaml") || p.Vars[Env] != "1" {
		t.Errorf("vars %v: the session is not pointed at the config's target", p.Vars)
	}
	for _, name := range []string{"GIT_CONFIG_GLOBAL", "XDG_CONFIG_HOME"} {
		if _, ok := p.Vars[name]; ok {
			t.Errorf("vars %v name a %s that is no symlink", p.Vars, name)
		}
	}
	if !p.Readable(filepath.Join(dotfiles, "config.yaml"), home) {
		t.Error("the config's target is not readable")
	}
	if p.Readable(filepath.Join(home, "projects/dotfiles/.ssh"), home) {
		t.Error("the target's neighbours opened")
	}
}

func TestASymlinkedConfigHomeIsNamedByItsTarget(t *testing.T) {
	home := testHome(t)
	dotfiles := filepath.Join(home, "projects/dotfiles/.config")
	if err := os.MkdirAll(filepath.Join(dotfiles, "git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dotfiles, "git/ignore"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dotfiles, filepath.Join(home, ".config")); err != nil {
		t.Fatal(err)
	}
	p := New(config.Sandbox{}, Paths{Home: home, ConfigFile: filepath.Join(home, ".config/beekeeper/config.yaml"), StateDir: filepath.Join(home, ".local/state/beekeeper")})
	if got := p.Vars["XDG_CONFIG_HOME"]; got != dotfiles {
		t.Errorf("XDG_CONFIG_HOME = %q, want the config directory's target %s", got, dotfiles)
	}
	if !slices.Contains(p.Read, filepath.Join(dotfiles, "git")) || !p.Readable(filepath.Join(dotfiles, "git/ignore"), home) {
		t.Errorf("read %v: git's config directory is not readable at its target", p.Read)
	}
	if p.Readable(filepath.Join(dotfiles, "gh/hosts.yml"), home) {
		t.Error("the config directory's neighbours of git opened")
	}
}
