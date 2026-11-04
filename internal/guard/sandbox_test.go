package guard

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/sandbox"
)

const searchSandbox = "sandbox"

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

// Both hook entry points must confine recursive tools without relying on a
// later Read call. All fixture contents are public canaries.
func TestSandboxRecursiveSearchConfinement(t *testing.T) {
	h, home := sandboxHook(t)
	base := filepath.Dir(home)
	runtime := filepath.Join(base, "runtime", "user")
	state := filepath.Join(home, ".local/state/beekeeper")
	scan := filepath.Join(state, "scan")
	checkout := filepath.Join(home, "projects/repo")
	h.Sandbox.Runtime = runtime
	h.Sandbox.Deny = []string{scan}
	for _, dir := range []string{runtime, scan, checkout, filepath.Join(home, ".private")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "canary.txt"), []byte("public search canary"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []string{"normal", searchSandbox} {
		t.Run(entry, func(t *testing.T) {
			for _, tc := range []struct {
				name, root string
				deny       bool
			}{
				{"above-home", base, true},
				{"above-runtime", filepath.Dir(runtime), true},
				{"state-deny-child", state, true},
				{"checkout", checkout, false},
				{"default-checkout", "", false},
			} {
				for _, tool := range []string{grepTool, globTool, "absolute-glob"} {
					t.Run(tc.name+"/"+tool, func(t *testing.T) {
						input := map[string]any{pathKey: tc.root, patternKey: "canary"}
						if tool == globTool {
							input[patternKey] = "**/*"
						}
						toolName := tool
						if tool == "absolute-glob" {
							toolName = globTool
							root := tc.root
							if root == "" {
								root = checkout
							}
							input = map[string]any{patternKey: filepath.Join(root, "**/*")}
						}
						ev := toolEvent(toolName, input)
						ev["cwd"] = checkout
						raw, err := json.Marshal(ev)
						if err != nil {
							t.Fatal(err)
						}
						var out []byte
						if entry == searchSandbox {
							out = h.DecideSandbox(raw)
						} else {
							out = h.Decide(raw)
						}
						if denied := strings.Contains(string(out), `"permissionDecision":"deny"`); denied != tc.deny {
							t.Fatalf("denied = %v, want %v: %s", denied, tc.deny, out)
						}
					})
				}
			}
			for _, tc := range []struct {
				path string
				deny bool
			}{
				{filepath.Join(state, "public.txt"), false},
				{filepath.Join(checkout, "canary.txt"), false},
				{filepath.Join(scan, "canary.txt"), true},
			} {
				ev := toolEvent(readTool, map[string]any{filePathKey: tc.path})
				ev["cwd"] = checkout
				raw, _ := json.Marshal(ev)
				var out []byte
				if entry == searchSandbox {
					out = h.DecideSandbox(raw)
				} else {
					out = h.Decide(raw)
				}
				if denied := strings.Contains(string(out), `"permissionDecision":"deny"`); denied != tc.deny {
					t.Fatalf("single-file read denied = %v, want %v: %s", denied, tc.deny, out)
				}
			}
		})
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

func TestFreshLabsBeforeAHoldersCommand(t *testing.T) {
	var got [][]string
	h := hook()
	h.Labs = func() []lease.Holder {
		return []lease.Holder{{Env: labB, Session: "s7"}, {Env: labA, HostSession: "local_s7"}, {Env: "lab-c", Session: "other"}}
	}
	h.FreshLabs = func(labs []string) { got = append(got, labs) }
	run := func(tool, session string) {
		ev := toolEvent(tool, map[string]any{commandKey: "kubectl get nodes"})
		ev["session_id"] = session
		decideEvent(t, h, ev)
	}
	run(bashTool, "s7")
	if len(got) != 1 || !slices.Equal(got[0], []string{labA, labB}) {
		t.Errorf("a lab lease's holder, unsandboxed: %q", got)
	}
	got = nil
	run(bashTool, "s2")
	run("Read", "s7")
	if got != nil {
		t.Errorf("no holder's Bash call: %q", got)
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
	for _, cmd := range []string{labUp, "cd lab && agentlab down", "agentlab --lab agentlab-2 up --open=false", "kind create cluster --name x", "kind delete cluster --name x", "kind delete clusters --all"} {
		if d := run(h, cmd); d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "beekeeper lease up <lab>") {
			t.Errorf("%s: %+v", cmd, d)
		}
	}
	for _, cmd := range []string{"agentlab status", "kind get clusters", "echo agentlab up", "echo 'x; agentlab down'",
		"python3 - <<'EOF'\nprint('''\nagentlab down\n''')\nEOF"} {
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

func TestDecideUnsandboxed(t *testing.T) {
	for _, c := range []struct {
		in     string
		refuse bool
	}{
		{`{"tool_name":"Bash","tool_input":{"command":"ls"}}`, true},
		{`{"tool_name":"Read","tool_input":{"file_path":"/etc/hosts"}}`, true},
		{`{"tool_name":"mcp__x__y","tool_input":{}}`, true},
		{`{"tool_name":"SendMessage","tool_input":{"to":"the supervisor"}}`, false},
		{`{`, false},
	} {
		out := DecideUnsandboxed([]byte(c.in))
		if refused := strings.Contains(string(out), `"permissionDecision":"deny"`); refused != c.refuse {
			t.Errorf("%s: %s, want refused %v", c.in, out, c.refuse)
		}
	}
	var start map[string]hookOutput
	if err := json.Unmarshal(UnsandboxedStart(), &start); err != nil || start["hookSpecificOutput"].HookEventName != "SessionStart" ||
		!strings.Contains(start["hookSpecificOutput"].AdditionalContext, "beekeeper sandbox install") {
		t.Errorf("start: %+v %v", start, err)
	}
}
