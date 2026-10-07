package guard

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
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
		{grepTool, map[string]any{pathKey: filepath.Join(home, ".config")}, refuseRead},
		{grepTool, map[string]any{}, ""}, // the working directory
		{globTool, map[string]any{patternKey: filepath.Join(home, ".ssh") + "/*"}, refuseRead},
		{globTool, map[string]any{patternKey: "**/*.go"}, ""},
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

func TestLabProxyRefreshesHeldLabs(t *testing.T) {
	h, home := sandboxHook(t)
	h.Labs = func() []lease.Holder {
		return []lease.Holder{{Env: "lab-b", Session: "s7"}, {Env: "lab-a", HostSession: "local_s7"}, {Env: "lab-c", Session: "other"}}
	}
	refresh := self + " lease kubeconfig --refresh lab-a lab-b >/dev/null 2>&1; "
	run := func(h Hook, session, cmd string) *decision {
		ev := toolEvent(bashTool, map[string]any{commandKey: cmd})
		ev["cwd"], ev["session_id"] = home, session
		return decideEvent(t, h, ev)
	}
	if d := run(h, "s7", "kubectl get nodes"); d == nil || d.UpdatedInput[commandKey] != refresh+"kubectl get nodes" {
		t.Errorf("a lab lease's holder: %+v", d)
	}
	if d := run(h, "s7", "go test ./..."); d == nil || !strings.HasPrefix(d.UpdatedInput[commandKey].(string), refresh+ShellQuote(self)+" run -- ") {
		t.Errorf("the refresh goes in front of the build wrap: %+v", d)
	}
	if d := run(h, "s2", "kubectl get nodes"); d != nil {
		t.Errorf("a session that holds no lab: %+v", d)
	}
	unsandboxed := h
	unsandboxed.Sandbox = nil
	if d := run(unsandboxed, "s7", "kubectl get nodes"); d != nil {
		t.Errorf("outside the sandbox: %+v", d)
	}
	broken := h
	broken.ConfigErr = errors.New("bad")
	if d := run(broken, "s7", "kubectl get nodes"); d == nil || d.PermissionDecision != decisionDeny {
		t.Errorf("a refused call stays refused: %+v", d)
	}
}

func TestSandboxSendsLabRuntimeToTheHost(t *testing.T) {
	h, home := sandboxHook(t)
	h.Clusters = func() []string { return nil }
	run := func(h Hook, cmd string) *decision {
		ev := toolEvent(bashTool, map[string]any{commandKey: cmd})
		ev["cwd"] = home
		return decideEvent(t, h, ev)
	}
	for _, cmd := range []string{"agentlab up", "cd lab && agentlab down", "agentlab --lab agentlab-2 up --open=false", "kind create cluster --name x", "kind delete cluster --name x", "kind delete clusters --all"} {
		if d := run(h, cmd); d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "beekeeper lease up <lab>") {
			t.Errorf("%s: %+v", cmd, d)
		}
	}
	for _, cmd := range []string{"agentlab status", "kind get clusters", "echo agentlab up"} {
		if d := run(h, cmd); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("%s: refused: %s", cmd, d.Reason)
		}
	}
	unsandboxed := h
	unsandboxed.Sandbox = nil
	if d := run(unsandboxed, "agentlab down"); d != nil && d.PermissionDecision == decisionDeny {
		t.Errorf("outside the sandbox: refused: %s", d.Reason)
	}
}
