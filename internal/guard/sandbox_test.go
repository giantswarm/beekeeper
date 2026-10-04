package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/sandbox"
)

func sandboxHook(t *testing.T) (Hook, string) {
	t.Helper()
	home := testHome(t)
	if err := os.MkdirAll(filepath.Join(home, "projects/repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := sandbox.New(config.Sandbox{AllowRead: []string{filepath.Join(home, "projects")}},
		sandbox.Paths{Home: home, ConfigFile: filepath.Join(home, ".config/beekeeper/config.yaml"), StateDir: filepath.Join(home, ".local/state/beekeeper"),
			LeaseDir: filepath.Join(home, ".local/state/beekeeper/leases"), Exe: filepath.Join(home, ".go/bin/beekeeper")})
	h := hook()
	h.Sandbox = &p
	return h, home
}

// The refusals' verbs, with the start of the path they name.
const (
	refuseRead  = "read /"
	refuseWrite = "write /"
)

func TestSandboxHoldsFileTools(t *testing.T) {
	h, home := sandboxHook(t)
	cwd := filepath.Join(home, "projects/repo")
	for _, c := range []struct {
		tool   string
		input  map[string]any
		refuse string
	}{
		{readTool, map[string]any{filePathKey: filepath.Join(home, ".kube/config")}, refuseRead},
		{readTool, map[string]any{filePathKey: "go.mod"}, ""},
		{readTool, map[string]any{filePathKey: "/etc/hosts"}, ""},
		{writeTool, map[string]any{filePathKey: filepath.Join(home, "x.txt")}, refuseWrite},
		{writeTool, map[string]any{filePathKey: filepath.Join(cwd, "x.go")}, ""},
		{editTool, map[string]any{filePathKey: filepath.Join(home, ".bashrc")}, refuseWrite},
		{"NotebookEdit", map[string]any{"notebook_path": filepath.Join(home, "n.ipynb")}, refuseWrite},
		{grepTool, map[string]any{"path": filepath.Join(home, ".config")}, refuseRead},
		{grepTool, map[string]any{}, ""}, // the working directory
		{globTool, map[string]any{"pattern": filepath.Join(home, ".ssh") + "/*"}, refuseRead},
		{globTool, map[string]any{"pattern": "**/*.go"}, ""},
	} {
		ev := toolEvent(c.tool, c.input)
		ev["cwd"] = cwd
		d := decideEvent(t, h, ev)
		refused := d != nil && d.PermissionDecision == decisionDeny
		if refused != (c.refuse != "") || refused && !strings.Contains(d.Reason, "agent sandbox does not let this session "+c.refuse) {
			t.Errorf("%s %v: %+v, want refused %q", c.tool, c.input, d, c.refuse)
		}
	}
}

func TestNoSandboxPassesFileTools(t *testing.T) {
	ev := toolEvent(readTool, map[string]any{filePathKey: "/home/u/.kube/config"})
	if d := decideEvent(t, hook(), ev); d != nil {
		t.Errorf("without a sandbox: %+v", d)
	}
}

func TestDecideSandbox(t *testing.T) {
	h, home := sandboxHook(t)
	ev := toolEvent(readTool, map[string]any{filePathKey: filepath.Join(home, ".kube/config")})
	ev["cwd"] = home
	raw, _ := json.Marshal(ev)
	if out := (Hook{Sandbox: h.Sandbox}).DecideSandbox(raw); !strings.Contains(string(out), `"permissionDecision":"deny"`) {
		t.Errorf("out of scope: %s", out)
	}
	ev = toolEvent(bashTool, map[string]any{"command": "cat ~/.kube/config"})
	ev["cwd"] = home
	raw, _ = json.Marshal(ev)
	if out := (Hook{Sandbox: h.Sandbox}).DecideSandbox(raw); out != nil {
		t.Errorf("Bash is the sandbox runtime's: %s", out)
	}
}

func TestGlobRoot(t *testing.T) {
	for in, want := range map[string]string{"/home/u/.ssh/*": "/home/u/.ssh", "/home/u/**/id_*": "/home/u", "/etc/os-release": "/etc/os-release", "/*": "/"} {
		if got := globRoot(in); got != want {
			t.Errorf("globRoot(%s) = %s, want %s", in, got, want)
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
