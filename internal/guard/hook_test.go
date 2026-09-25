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
		"cd ~/src/x && CGO_ENABLED=0 ~/bin/memcap -- go build ./...",
		`MEMCAP_WAIT=60m "$HOME/.go/bin/beekeeper" run -- zsh -c 'cd x; go test ./...'`,
		"if [ -x scripts/memcap ]; then scripts/memcap graphify update .; fi",
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
	if d == nil || d.PermissionDecision != decisionAllow {
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
	for _, c := range []string{"make build", "timeout 600 go vet ./...", "x=$(yarn tsc)", "if true; then golangci-lint run; fi", "FOO=1 npx jest",
		// the wrapper mentioned, not invoked: the build still needs a slot
		"cd ~/d && m=$(ls ~/klaus-lab/scripts/memcap 2>/dev/null); echo $m; go test ./e2e/",
		"which memcap beekeeper && go vet ./...",
		// invoked through a variable: wrapped again, the inner run takes no slot
		`go vet ./... && M=/x/scripts/memcap && "$M" go test ./...`,
	} {
		if decide(t, hook(), "/", c, nil) == nil {
			t.Errorf("%q: want wrapped", c)
		}
	}
}

func TestBackgroundRunGetsTheLongWait(t *testing.T) {
	d := decide(t, hook(), "/", "yarn test --maxWorkers=4", map[string]any{backgroundKey: true})
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
	if d == nil || d.PermissionDecision != decisionDeny {
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
		if d := decide(t, h, lab, "agentlab up", nil); d == nil || d.PermissionDecision != decisionAllow {
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

func TestHookGatesMerges(t *testing.T) {
	h := Hook{Self: self, Clusters: func() []string { return nil }, Leases: func() []lease.Holder { return nil }}
	g := self + " gate -- "
	for _, c := range []struct {
		cmd, want string
		bg        bool
	}{
		{"devctl pr merge giantswarm/marge 3", g + "devctl pr merge giantswarm/marge 3", false},
		{"devctl pr merge giantswarm/marge 3 --timeout 9m | $L record x", g + "devctl pr merge giantswarm/marge 3 --timeout 9m | $L record x", false},
		{"timeout 600 devctl pr merge o/r 1", "timeout 600 " + g + "devctl pr merge o/r 1", false},
		{"cd x && devctl pr merge o/r 1", "cd x && " + g + "devctl pr merge o/r 1", false},
		{"devctl pr merge o/r 1", self + " gate --wait 30m -- devctl pr merge o/r 1", true},
		{"make && devctl pr merge o/r 1", self + " run -- zsh -c 'make && " + g + "devctl pr merge o/r 1'", false},
		// The live case: only the devctl invocation is wrapped, the pipeline runs as written.
		{`flock ~/.local/state/lab/locks/merge-marge.lock devctl pr merge --update-branch o/r 7 | tee "$(scripts/scratch orch merge-7.json)" | ./orchestration/ledger.sh record o/r#7 --agent orchestrator`,
			`flock ~/.local/state/lab/locks/merge-marge.lock ` + g + `devctl pr merge --update-branch o/r 7 | tee "$(scripts/scratch orch merge-7.json)" | ./orchestration/ledger.sh record o/r#7 --agent orchestrator`, false},
		{`gh pr view 7 --repo o/r --json state && (flock x.lock devctl pr merge o/r 7 | tee m.json)`,
			`gh pr view 7 --repo o/r --json state && (flock x.lock ` + g + `devctl pr merge o/r 7 | tee m.json)`, false},
		{"mkdir -p x && flock -n -x x.lock devctl pr merge o/r 7", "mkdir -p x && flock -n -x x.lock " + g + "devctl pr merge o/r 7", false},
		{"flock -w 600 x.lock devctl pr merge o/r 7", "flock -w 600 x.lock " + g + "devctl pr merge o/r 7", false},
		{"setsid nohup devctl pr merge o/r 7 >| m.json 2>&1", "setsid nohup " + g + "devctl pr merge o/r 7 >| m.json 2>&1", false},
		{"stdbuf -oL -eL devctl pr merge o/r 7 | tee m.json", "stdbuf -oL -eL " + g + "devctl pr merge o/r 7 | tee m.json", false},
		{"ionice -c3 nice -n 19 devctl pr merge o/r 7", "ionice -c3 nice -n 19 " + g + "devctl pr merge o/r 7", false},
		{"chrt -i 0 devctl pr merge o/r 7", "chrt -i 0 " + g + "devctl pr merge o/r 7", false},
		{"timeout -k 10 590 devctl pr merge o/r 7", "timeout -k 10 590 " + g + "devctl pr merge o/r 7", false},
		{"env -u GITHUB_TOKEN -u NVM_BIN DEVCTL_UNSAFE_FORCE_VERSION=8.85.8 devctl pr merge o/r 7",
			"env -u GITHUB_TOKEN -u NVM_BIN DEVCTL_UNSAFE_FORCE_VERSION=8.85.8 " + g + "devctl pr merge o/r 7", false},
		{`OUT="$S/m.json" devctl pr merge o/r 7 >| "$OUT"`, `OUT="$S/m.json" ` + g + `devctl pr merge o/r 7 >| "$OUT"`, false},
		// devctl by path.
		{"export V=1; ~/bin/devctl pr merge o/r 7", "export V=1; " + g + "~/bin/devctl pr merge o/r 7", false},
		{"cd ~/.local/state/d && ./devctl pr merge o/r 7", "cd ~/.local/state/d && " + g + "./devctl pr merge o/r 7", false},
		{"for x in 1 2; do $HOME/bin/devctl pr merge o/r $x; done", "for x in 1 2; do " + g + "$HOME/bin/devctl pr merge o/r $x; done", false},
		// A merge on a line of its own in a multi-line -c string is gated in place.
		{"bash -c '\ntrap restore EXIT\ndevctl pr merge o/r 7\n'", "bash -c '\ntrap restore EXIT\n" + g + "devctl pr merge o/r 7\n'", false},
	} {
		d := decide(t, h, t.TempDir(), c.cmd, map[string]any{backgroundKey: c.bg})
		if d == nil || d.UpdatedInput["command"] != c.want {
			t.Errorf("%q: got %+v, want %q", c.cmd, d, c.want)
			continue
		}
		if _, ok := d.UpdatedInput["timeout"]; ok == c.bg {
			t.Errorf("%q: timeout %v in the background=%v", c.cmd, d.UpdatedInput["timeout"], c.bg)
		}
	}
	for _, cmd := range []string{"devctl pr wait o/r 1", "devctl version", "echo devctl pr merge o/r 1", g + "devctl pr merge o/r 1",
		"flock x.lock " + g + "~/bin/devctl pr merge o/r 1", "sed -i 's/gs-pr-merge/devctl pr merge/g' f", "pgrep -af 'devctl pr merge'"} {
		if d := decide(t, h, t.TempDir(), cmd, nil); d != nil {
			t.Errorf("%q is rewritten: %v", cmd, d.UpdatedInput["command"])
		}
	}
}

// TestHookGatesRealMerges feeds every devctl pr merge statement from the
// transcripts of the machine the hook runs on, reduced to its shell skeleton
// (words outside the shell structure are x, repositories o/r), with the
// number of merges a shell parser finds in it. Each merge is gated, and the
// rewrite adds nothing but the gates.
func TestHookGatesRealMerges(t *testing.T) {
	h := Hook{Self: self, Clusters: func() []string { return nil }, Leases: func() []lease.Holder { return nil }}
	raw, err := os.ReadFile("testdata/merges.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		var c struct {
			Command string `json:"command"`
			Merges  int    `json:"merges"`
		}
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatal(err)
		}
		n++
		d := decide(t, h, t.TempDir(), c.Command, nil)
		if d == nil || d.PermissionDecision != decisionAllow {
			t.Errorf("%q: not gated: %+v", c.Command, d)
			continue
		}
		got, _ := d.UpdatedInput["command"].(string)
		if strings.HasPrefix(got, self+" run -- zsh -c ") {
			got = strings.ReplaceAll(got, `'"'"'`, `'`)
			got = strings.TrimSuffix(strings.TrimPrefix(got, self+" run -- zsh -c '"), "'")
		}
		// A mention after a | or ; inside a quoted string is gated too (a
		// grep pattern): more gates than merges only where there are mentions.
		k := strings.Count(got, self+" gate -- ")
		if k < c.Merges || k > c.Merges && strings.Count(c.Command, "devctl pr merge") == c.Merges {
			t.Errorf("%q: %d gates, want %d: %s", c.Command, k, c.Merges, got)
		}
		if strings.ReplaceAll(got, self+" gate -- ", "") != c.Command {
			t.Errorf("%q: rewritten beyond the gate: %s", c.Command, got)
		}
	}
	if n < 500 {
		t.Errorf("testdata holds %d merges, want the whole corpus", n)
	}
}

func TestHookRefusesHiddenMerges(t *testing.T) {
	h := Hook{Self: self, Clusters: func() []string { return nil }, Leases: func() []lease.Holder { return nil }}
	g := self + " gate -- "
	for _, c := range []struct {
		cmd, fixed string
		bg         bool
	}{
		{`bash -c 'devctl pr merge o/r 7' | tee m.json`, `bash -c '` + g + `devctl pr merge o/r 7' | tee m.json`, false},
		{`zsh -lc "flock x.lock ~/bin/devctl pr merge o/r 7 >| \"$S/m.json\""`, `zsh -lc "flock x.lock ` + g + `~/bin/devctl pr merge o/r 7 >| \"$S/m.json\""`, false},
		{`timeout 600 sh -c "devctl pr merge o/r 7"`, `timeout 600 sh -c "` + self + ` gate --wait 30m -- devctl pr merge o/r 7"`, true},
		{self + ` run -- zsh -c 'devctl pr merge o/r 7'`, self + ` run -- zsh -c '` + g + `devctl pr merge o/r 7'`, false},
	} {
		d := decide(t, h, t.TempDir(), c.cmd, map[string]any{backgroundKey: c.bg})
		if d == nil || d.PermissionDecision != decisionDeny || !strings.HasSuffix(d.Reason, "\n"+c.fixed) {
			t.Errorf("%q: got %+v, want a refusal naming %q", c.cmd, d, c.fixed)
			continue
		}
		// The gated form it names passes.
		if d := decide(t, h, t.TempDir(), c.fixed, map[string]any{backgroundKey: c.bg}); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("%q: the gated form is refused too: %s", c.fixed, d.Reason)
		}
	}
	for _, cmd := range []string{`bash -c 'devctl pr wait o/r 7'`, `bash -c 'echo ok' && echo "devctl pr merge o/r 7"`, `ssh -c aes x`} {
		if d := decide(t, h, t.TempDir(), cmd, nil); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("%q is refused: %s", cmd, d.Reason)
		}
	}
}
