package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/lease"
)

const self = "/home/u/.go/bin/beekeeper"

type decision struct {
	PermissionDecision string         `json:"permissionDecision"`
	Reason             string         `json:"permissionDecisionReason"`
	UpdatedInput       map[string]any `json:"updatedInput"`
}

// decide feeds a Bash tool call to the hook the way Claude Code sends it.
func decide(t *testing.T, h Hook, cwd, command string, extra map[string]any) *decision {
	t.Helper()
	ti := map[string]any{"command": command}
	for k, v := range extra {
		ti[k] = v
	}
	raw, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": ti, "cwd": cwd})
	out := h.Decide(raw)
	if out == nil {
		return nil
	}
	var o struct {
		D decision `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &o); err != nil {
		t.Fatalf("hook output %q: %v", out, err)
	}
	return &o.D
}

func hook(clusters ...string) Hook {
	return Hook{Self: self, Clusters: func() []string { return clusters },
		Leases: func() []lease.Holder {
			return []lease.Holder{{Env: "agentlab-1", Name: "Agent one", Purpose: "kagent e2e", Since: "2026-09-24T10:00:00Z"}}
		}}
}

func TestPassThrough(t *testing.T) {
	for _, cmd := range []string{
		"git status --short && ls -la",
		"tsc --version",
		"/home/u/klaus-lab/scripts/memcap -- zsh -c 'go test ./...'",
		self + " run -- zsh -c 'go test ./...'",
		"beekeeper run --max 4G -- go test ./...",
		"make help",
		"make -n build",
		"echo 'go test ./...'",
		"   ",
	} {
		if d := decide(t, hook(), "/", cmd, nil); d != nil {
			t.Errorf("%q: want pass-through, got %+v", cmd, d)
		}
	}
	for _, in := range []string{"", "{", "[]", `{"tool_name":"Read","tool_input":{"command":"go test"}}`, `{"tool_name":"Bash","tool_input":{"command":7}}`} {
		if out := hook().Decide([]byte(in)); out != nil {
			t.Errorf("%q: want pass-through, got %s", in, out)
		}
	}
}

func TestHeavyCommandIsWrappedVerbatim(t *testing.T) {
	cmd := "cd ~/src/x && export GOFLAGS=-mod=vendor && go test -p 2 ./... 2>&1 | tail -40"
	d := decide(t, hook(), "/", cmd, map[string]any{"timeout": 120000, "description": "tests"})
	if d == nil || d.PermissionDecision != "allow" {
		t.Fatalf("want allow, got %+v", d)
	}
	want := self + " run -- zsh -c " + ShellQuote(cmd)
	if got := d.UpdatedInput["command"]; got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
	if got := d.UpdatedInput["timeout"]; got != float64(600000) {
		t.Errorf("timeout = %v", got)
	}
	if d.UpdatedInput["description"] != "tests" {
		t.Errorf("other tool input fields lost: %v", d.UpdatedInput)
	}
	for _, c := range []string{"make build", "timeout 600 go vet ./...", "x=$(yarn tsc)", "if true; then golangci-lint run; fi", "FOO=1 npx jest"} {
		if decide(t, hook(), "/", c, nil) == nil {
			t.Errorf("%q: want wrapped", c)
		}
	}
}

func TestBackgroundRunGetsTheLongWait(t *testing.T) {
	d := decide(t, hook(), "/", "yarn test --maxWorkers=4", map[string]any{"run_in_background": true})
	if d == nil || !strings.HasPrefix(d.UpdatedInput["command"].(string), "MEMCAP_WAIT=60m ") {
		t.Fatalf("want the 60m wait, got %+v", d)
	}
	if _, ok := d.UpdatedInput["timeout"]; ok {
		t.Errorf("a background run keeps its timeout: %v", d.UpdatedInput)
	}
}

func TestThirdLabIsRefused(t *testing.T) {
	lab := t.TempDir()
	if err := os.WriteFile(filepath.Join(lab, "agentlab.yaml"), []byte("clusterName: \"agentlab-2\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := decide(t, hook("agentlab-1", "agentlab-2"), "/", "kind create cluster --name spare", nil)
	if d == nil || d.PermissionDecision != "deny" {
		t.Fatalf("want deny, got %+v", d)
	}
	for _, want := range []string{"2 kind labs already run (agentlab-1, agentlab-2)", "would create a third (spare)", "agentlab-1: Agent one since 2026-09-24T10:00:00Z — kagent e2e"} {
		if !strings.Contains(d.Reason, want) {
			t.Errorf("reason lacks %q:\n%s", want, d.Reason)
		}
	}
	if d := decide(t, hook("agentlab-1", "agentlab-2"), "/", "cd /nowhere && agentlab up", nil); d == nil || !strings.Contains(d.Reason, "a new lab in /nowhere") {
		t.Errorf("a new lab directory: want deny, got %+v", d)
	}
	// Re-running a lab that runs, or a second lab, is fine (and wrapped: agentlab up is heavy).
	for _, h := range []Hook{hook("agentlab-1", "agentlab-2"), hook("agentlab-1")} {
		if d := decide(t, h, lab, "agentlab up", nil); d == nil || d.PermissionDecision != "allow" {
			t.Errorf("want allow, got %+v", d)
		}
	}
	h := hook("a", "b")
	h.Leases = func() []lease.Holder { return nil }
	if d := decide(t, h, "/", "kind create cluster", nil); d == nil || !strings.HasSuffix(d.Reason, "leases:\nnone held") {
		t.Errorf("no leases: got %+v", d)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{"": "''", "/a/b-c.d": "/a/b-c.d", "a b": "'a b'", "it's": `'it'"'"'s'`} {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParse(t *testing.T) {
	for in, want := range map[string]int64{"12G": 12 << 20, "512m": 512 << 10, "64K": 64, "1T": 1 << 30, "2048": 2} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "12GB", "-1", "infinity"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q): want an error", in)
		}
	}
	for in, want := range map[string]string{"8m": "8m0s", "90": "1m30s", "1h": "1h0m0s"} {
		if got, err := ParseWait(in); err != nil || got.String() != want {
			t.Errorf("ParseWait(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
}
