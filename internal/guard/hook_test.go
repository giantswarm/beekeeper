package guard

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/lease"
)

const self = "/home/u/.go/bin/beekeeper"

type decision struct {
	PermissionDecision string         `json:"permissionDecision"`
	Reason             string         `json:"permissionDecisionReason"`
	UpdatedInput       map[string]any `json:"updatedInput"`
	AdditionalContext  string         `json:"additionalContext"`
}

// decide feeds a Bash tool call to the hook the way Claude Code sends it.
func decide(t *testing.T, h Hook, cwd, command string, extra map[string]any) *decision {
	t.Helper()
	ti := map[string]any{"command": command}
	for k, v := range extra {
		ti[k] = v
	}
	ev := toolEvent(bashTool, ti)
	ev["cwd"] = cwd
	return decideEvent(t, h, ev)
}

// decideEvent feeds any PreToolUse event to the hook.
func decideEvent(t *testing.T, h Hook, ev map[string]any) *decision {
	t.Helper()
	raw, _ := json.Marshal(ev)
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

// testShell is the shell the tests' hook rewrites into.
const testShell = "zsh"

// labUp creates a lab.
const labUp = "agentlab up"

func hook(clusters ...string) Hook {
	return Hook{Self: self, Shell: testShell, MaxLabs: func() int { return 2 }, Production: "gazelle",
		ContextHint: "teleport.giantswarm.io-<installation>", Clusters: func() []string { return clusters },
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
		"beekeeper scan sweep --json",
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

func TestHeavyClassification(t *testing.T) {
	for _, tc := range []struct {
		cmd   string
		heavy bool
	}{
		// lab commands: the lab-count guard and the lab lease govern them
		{"agentlab platform up", false},
		{labUp, false},
		{"agentlab platform", false},
		{"agentlab test", false},
		{"agentlab platform-test", false},
		{"agentlab backstage-test", false},
		// the package-script form of a lab e2e proof
		{"yarn test:e2e:agentlab", false},
		{"cd ~/src/backstage && AGENTLAB_KUBECONFIG=~/lab/kubeconfig yarn test:e2e:agentlab 2>&1 | tail -40", false},
		{"npm run test:e2e:agentlab", false},
		{"yarn workspace app test:e2e:agentlab", false},
		{"yarn test:e2e:agentlab && yarn tsc", true},
		{"yarn test:e2e", true},
		{"agentlab down", false},
		{"cd ~/lab && agentlab --lab agentlab-1 substrate-step-test --to 0.9.0 2>&1 | tail -40", false},
		// graphify: the AST-only update and the LLM steps build nothing
		{"graphify update .", false},
		{"cd ~/src/x && graphify update . && graphify query 'who calls isHeavy'", false},
		{"graphify label graphify-out", false},
		{"graphify cluster-only .", false},
		// read-only pipelines whose build word is an argument
		{"gh pr checks 1 --repo o/r | grep -i 'step-test'", false},
		{"gh pr checks 1 --repo o/r | grep -E 'lint|make|go test|test'", false},
		{"gh run view 5 --repo o/r --log | grep -E \"FAIL|make: \\*\\*\\*\" | head", false},
		{"gh pr view 3 --repo o/r --json statusCheckRollup | jq '.statusCheckRollup[] | select(.name|test(\"build|lint\"))'", false},
		{"kubectl --context kind-a get pods -A | grep -v 'Running|make'", false},
		{"kubectl --context kind-a logs deploy/x | grep -c 'go test'", false},
		{"git log --oneline | grep -E 'fix|go build'", false},
		{"python3 - <<'EOF'\nprint('go test ./...')\nEOF", false},
		{"cat > notes.md <<EOF\nmake build\nEOF", false},
		{"echo 'bash -c \"make\"'", false},
		// git's working-tree commands build nothing, whatever their branch or
		// path names say
		{`git -C "$S" checkout -q -- .`, false},
		{"git -C /tmp/amp checkout -q --detach e82d26b", false},
		{"git checkout -q -b fix-make-test origin/main", false},
		{"git switch -c go-test-flake", false},
		{"git worktree add -q ~/projects/worktrees/x/make-build -b make-build origin/main", false},
		{"cd ~/src/x && git fetch -q origin && git checkout -q -B build origin/main", false},
		{`S=/tmp/amp; git -C "$S" checkout -q -- . && git -C ~/src/x worktree remove "$S"`, false},
		// builds, tests and lints keep the slot
		{"go test ./...", true},
		{"make lint", true},
		{"make", true},
		{"docker build .", true},
		{"docker buildx build -t x .", true},
		{"golangci-lint run ./...", true},
		{"npm ci", true},
		{"yarn test", true},
		{"ko build ./cmd/x", true},
		{"kind load docker-image x", true},
		{"graphify extract .", true},
		{"go test ./... 2>&1 | grep -E 'FAIL|ok'", true},
		{"gh pr checks 1 --repo o/r && make test", true},
		{"zsh -c 'go test ./...'", true},
		{`bash -c "cd x && make build"`, true},
		{"eval 'golangci-lint run'", true},
		{"bash <<'EOF'\ngo test ./...\nEOF", true},
		{"x=\"$(make image)\"", true},
		// a build after a git command, at each position the hook keys on
		{"git checkout -q main && go test ./...", true},
		{"git switch -q main; make build", true},
		{"git worktree add -q ../w -b b origin/main\ncd ../w && go build ./...", true},
		{`S=/tmp/amp; git -C "$S" checkout -q -- .; cd x && go test -race -count=150 ./internal/lab/`, true},
		{"git checkout -q main || golangci-lint run", true},
		{"git checkout -q main | timeout 600 go vet ./...", true},
		{"if git checkout -q main; then make test; fi", true},
		{"git checkout -q main && (cd x && GOFLAGS=-mod=mod go build ./...)", true},
		// a help or version flag builds nothing, whatever the command word
		{"govulncheck -h", false},
		{"golangci-lint --version", false},
		{"go version", false},
		{"go help test", false},
		{"go env GOPATH", false},
		{"govulncheck -version", false},
		{"cd ~/src/x && govulncheck -h 2>&1 | head -30", false},
		{"go test --help", false},
		{"golangci-lint run --help > /dev/null", false},
		{"bash -c 'golangci-lint help'", false},
		{"govulncheck ./...", true},
		{"govulncheck -h ./...", true},
		{"go test ./... --help", true},
		{"golangci-lint --version && golangci-lint run", true},
		{"go version && go build ./...", true},
		// a -c or eval string is read as the shell builds it: adjacent quoted
		// pieces joined, $'…' decoded
		{`bash -c 'cd x && '"go test ./..."`, true},
		{`sh -c "cd x; "'make build'`, true},
		{`bash -c 'cd x; 'go\ vet\ ./...`, true},
		{`bash -c $'cd x\ngo test ./...'`, true},
		{`zsh -c 'cd x;'$'\n\x67o build ./...'`, true},
		{`eval 'cd x;' go test ./...`, true},
		{`bash -c 'echo "$@"' _ go test ./...`, false},
		{`bash -c 'echo '"'go test ./...'"`, false},
		// $'…' ends at its unescaped quote, not at an escaped one
		{`echo $'it\'s'; go test ./...`, true},
		{`echo $'it\'s; go test ./...'`, false},
		{`echo $$'\'; go test ./...`, true},
		{`bash -c 'cd x;'$$'\'; go vet ./...`, true},
	} {
		if got := isHeavy(tc.cmd); got != tc.heavy {
			t.Errorf("isHeavy(%q) = %v, want %v", tc.cmd, got, tc.heavy)
		}
		d := decide(t, hook(), "/", tc.cmd, nil)
		c := ""
		if d != nil {
			c, _ = d.UpdatedInput["command"].(string)
		}
		if wrappedRun := strings.Contains(c, " run -- "); wrappedRun != tc.heavy {
			t.Errorf("%q: wrapped = %v, want %v (%+v)", tc.cmd, wrappedRun, tc.heavy, d)
		}
	}
}

// Every background call exports BackgroundEnv, so that a beekeeper run in
// it, however reached, waits the background default; a foreground call's
// run, direct or chained, passes unchanged and waits the foreground default.
func TestBackgroundRunGetsTheLongWait(t *testing.T) {
	for _, cmd := range []string{
		"yarn test --maxWorkers=4",
		"beekeeper run -- go test ./...",
		"cd /src/x && export GOFLAGS=-p=2 && beekeeper run -- go test ./...",
		"./scripts/build.sh",
		"git status",
	} {
		d := decide(t, hook(), "/", cmd, map[string]any{backgroundKey: true})
		if d == nil || !strings.HasPrefix(d.UpdatedInput["command"].(string), "export MEMCAP_BACKGROUND=1; ") {
			t.Errorf("%q in the background: want the background export, got %+v", cmd, d)
			continue
		}
		if _, ok := d.UpdatedInput["timeout"]; ok {
			t.Errorf("%q: a background run keeps its timeout: %v", cmd, d.UpdatedInput)
		}
	}
	for _, cmd := range []string{
		"beekeeper run -- go test ./...",
		"cd /src/x && export GOFLAGS=-p=2 && beekeeper run -- go test ./...",
	} {
		if d := decide(t, hook(), "/", cmd, nil); d != nil {
			t.Errorf("%q in the foreground: want it unchanged, got %+v", cmd, d)
		}
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
	for _, want := range []string{"2 kind labs already run (agentlab-1, agentlab-2)", "would create another (spare)", "agentlab-1: Agent one since 2026-09-24T10:00:00Z — kagent e2e"} {
		if !strings.Contains(d.Reason, want) {
			t.Errorf("reason lacks %q:\n%s", want, d.Reason)
		}
	}
	if d := decide(t, hook("agentlab-1", "agentlab-2"), "/", "cd /nowhere && agentlab up", nil); d == nil || !strings.Contains(d.Reason, "a new lab in /nowhere") {
		t.Errorf("a new lab directory: want deny, got %+v", d)
	}
	// Re-running a lab that runs, or a second lab, is fine, and no build: it passes unwrapped.
	for _, h := range []Hook{hook("agentlab-1", "agentlab-2"), hook("agentlab-1")} {
		if d := decide(t, h, lab, labUp, nil); d != nil {
			t.Errorf("want pass-through, got %+v", d)
		}
	}
	h := hook("a", "b")
	h.Leases = func() []lease.Holder { return nil }
	if d := decide(t, h, "/", "kind create cluster", nil); d == nil || !strings.HasSuffix(d.Reason, "leases:\nnone held") {
		t.Errorf("no leases: got %+v", d)
	}
}

// TestLabGuardReadsCommands: the lab guard refuses a lab command the shell
// runs, never the words of a here-document, a quoted string or a script's
// string literal.
func TestLabGuardReadsCommands(t *testing.T) {
	for _, tc := range []struct {
		name, cmd string
		refused   bool
	}{
		{"a here-document's text", "python3 - <<'EOF'\nimport pathlib\np = pathlib.Path('docs/lab.md')\np.write_text('''Start it:\n\nagentlab up\n''')\nEOF", false},
		{"a file written from a here-document", "cat > notes.md <<EOF\nagentlab up\nkind create cluster\nEOF", false},
		{"a quoted string", `echo "run agentlab up first"`, false},
		{"a quoted string after a separator", `git commit -m "x; agentlab up"`, false},
		{"a python literal", `python3 -c 'import subprocess; print("agentlab up")'`, false},
		{"a grep pattern", "grep -rn 'agentlab up' docs/", false},
		{"the command", labUp, true},
		{"after a separator", "x; agentlab up", true},
		{"after &&", "cd /nowhere && agentlab up", true},
		{"in a pipeline", "true | agentlab up", true},
		{"a command substitution", "$(agentlab up)", true},
		{"a command substitution in double quotes", `echo "$(agentlab up)"`, true},
		{"in a subshell", "(agentlab up)", true},
		{"after then", "if true; then agentlab up; fi", true},
		{"a shell's -c string", "bash -c 'agentlab up'", true},
		{"a here-document a shell reads", "bash <<'EOF'\nagentlab up\nEOF", true},
		{"kind create cluster", "kind create cluster --name spare", true},
		{"its help after a separator", "cd ~/lab && agentlab up --help", false},
		{"kind create cluster's help", "kind create cluster -h | head", false},
	} {
		d := decide(t, hook("a", "b"), "/", tc.cmd, nil)
		refused := d != nil && d.PermissionDecision == decisionDeny
		if refused != tc.refused {
			t.Errorf("%s: %q refused = %v, want %v (%+v)", tc.name, tc.cmd, refused, tc.refused, d)
		}
		if refused && !strings.Contains(d.Reason, "kind labs already run") {
			t.Errorf("%s: refused by another guard: %s", tc.name, d.Reason)
		}
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
	h := Hook{Self: self, Shell: testShell, Clusters: func() []string { return nil }, Leases: func() []lease.Holder { return nil }}
	g := self + " gate -- "
	for _, c := range []struct {
		cmd, want string
		bg        bool
	}{
		{"devctl pr merge giantswarm/marge 3", g + "devctl pr merge giantswarm/marge 3", false},
		{"devctl pr merge giantswarm/marge 3 --timeout 9m | $L record x", g + "devctl pr merge giantswarm/marge 3 --timeout 9m | $L record x", false},
		{"timeout 600 devctl pr merge o/r 1", "timeout 600 " + g + "devctl pr merge o/r 1", false},
		{"cd x && devctl pr merge o/r 1", "cd x && " + g + "devctl pr merge o/r 1", false},
		{"devctl pr merge o/r 1", backgroundExport + self + " gate --wait 30m --limit 0 -- devctl pr merge o/r 1", true},
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
		// The blocking waits are gated too.
		{"devctl pr wait o/r 1", g + "devctl pr wait o/r 1", false},
		{"devctl release wait o/r --pr 1 | tail -3", g + "devctl release wait o/r --pr 1 | tail -3", false},
		{"devctl rollout wait inst o/r --pr 1 >| r.json", g + "devctl rollout wait inst o/r --pr 1 >| r.json", false},
		{"devctl pr wait o/r 1", backgroundExport + self + " gate --wait 30m --limit 0 -- devctl pr wait o/r 1", true},
		// A merge on a line of its own in a multi-line -c string is gated in place.
		{"bash -c '\ntrap restore EXIT\ndevctl pr merge o/r 7\n'", "bash -c '\ntrap restore EXIT\n" + g + "devctl pr merge o/r 7\n'", false},
		// An indented line is a command position too.
		{"for pr in 1 2\ndo\n  echo $pr\n  devctl pr merge o/r $pr\ndone", "for pr in 1 2\ndo\n  echo $pr\n  " + g + "devctl pr merge o/r $pr\ndone", false},
		// A here-document a shell reads is a command line: gated in place, like the -c string.
		{"bash <<'EOF'\nset -e\n  devctl pr merge o/r 7\nEOF", "bash <<'EOF'\nset -e\n  " + g + "devctl pr merge o/r 7\nEOF", false},
		{"cat <<'EOF' | sh\ndevctl release promote o/r\nEOF", "cat <<'EOF' | sh\n" + g + "devctl release promote o/r\nEOF", false},
		{"eval devctl pr merge o/r 7", "eval " + g + "devctl pr merge o/r 7", false},
		// A real merge beside a quoted argument naming one: only the real one is gated.
		{`gh pr comment 7 --repo o/r --body 'Next: x; devctl pr merge o/r 7' && devctl pr merge o/r 7`,
			`gh pr comment 7 --repo o/r --body 'Next: x; devctl pr merge o/r 7' && ` + g + `devctl pr merge o/r 7`, false},
		{`grep -E '(x & devctl pr wait o/r 7|y; devctl pr merge)' log.txt; devctl pr wait o/r 7`,
			`grep -E '(x & devctl pr wait o/r 7|y; devctl pr merge)' log.txt; ` + g + `devctl pr wait o/r 7`, false},
		// $'…' ends at its unescaped quote: what follows a text command's
		// $'…' argument is a command line again.
		{`echo $'a\''; devctl pr merge o/r 7; echo 'b'`, `echo $'a\''; ` + g + `devctl pr merge o/r 7; echo 'b'`, false},
		{`gh pr comment 7 --repo o/r --body $'it\'s\\'; devctl pr merge o/r 7 # 'x`,
			`gh pr comment 7 --repo o/r --body $'it\'s\\'; ` + g + `devctl pr merge o/r 7 # 'x`, false},
		// $$ is the pid: the quote after it is a plain one, no $'…'.
		{`echo $$'\'; devctl pr merge o/r 7; echo '\'`, `echo $$'\'; ` + g + `devctl pr merge o/r 7; echo '\'`, false},
		{`printf '%s\n' $'\''; devctl release promote o/r; echo '\'`,
			`printf '%s\n' $'\''; ` + g + `devctl release promote o/r; echo '\'`, false},
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
	for _, cmd := range []string{"devctl pr view o/r 1", "devctl release list o/r", "devctl version", "echo devctl pr merge o/r 1", g + "devctl pr merge o/r 1",
		"flock x.lock " + g + "~/bin/devctl pr merge o/r 1", "sed -i 's/gs-pr-merge/devctl pr merge/g' f", "pgrep -f 'devctl pr merge'",
		// A here-document that is content stays as written: an issue body, a file.
		"gh issue create --repo o/r --title t --body-file - <<'EOF'\nThe merge failed:\n\n    devctl pr merge o/r 7\nEOF",
		"cat <<'EOF' >| notes.md\ndevctl pr merge o/r 7\nEOF",
		// A quoted argument of another command is text after its ; & or ( too.
		`gh issue comment 1 --repo o/r --body "Run it: cd x; devctl pr merge o/r 7 (or & devctl pr wait o/r 7)"`,
		`jq -r 'select(.cmd | test("x & devctl pr wait o/r 7"))' log.json`,
		"git commit -m 'gate: x\n\n(devctl pr merge o/r 7)'",
		// The gate written in after a $'…' argument passes as written.
		`echo $'a\''; ` + g + `devctl pr merge o/r 7; echo 'b'`,
		// A $'…' argument with an escaped quote is text to its end.
		`echo $'it\'s done; devctl pr merge o/r 7'`,
		`gh pr comment 7 --repo o/r --body $'next:\n\tdevctl pr merge o/r 7 (it\'s gated)'`} {
		if d := decide(t, h, t.TempDir(), cmd, nil); d != nil {
			t.Errorf("%q is rewritten: %v", cmd, d.UpdatedInput["command"])
		}
	}
}

// TestHookRefusesScriptMerges: a merge the call runs from a script file, from
// a here-document or input an interpreter reads, or from an interpreter's
// code is refused, the refusal naming the place and the line; one a shell
// reads from a here-document is gated in place (TestHookGatesMerges).
func TestHookRefusesScriptMerges(t *testing.T) {
	h := Hook{Self: self, Shell: testShell, Clusters: func() []string { return nil }, Leases: func() []lease.Holder { return nil }}
	g := self + " gate -- "
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("merge.sh", "#!/usr/bin/env bash\nset -euo pipefail\n# devctl pr merge o/r 1 is run below\nfor pr in 7 8; do\n  devctl pr merge o/r $pr | tee \"m-$pr.json\"\ndone\n", 0o755)
	write("outer.sh", "echo start\nbash merge.sh\n", 0o644)
	write("gated.sh", "set -e\n"+g+"devctl pr merge o/r 7\n", 0o644)
	write("mention.sh", "echo 'run devctl pr merge o/r 7 yourself'\ngh pr view 7 --repo o/r\n", 0o644)
	write("merge.py", "#!/usr/bin/env python3\nimport subprocess\nsubprocess.run([\"devctl\", \"pr\", \"merge\", \"o/r\", \"7\"], check=True)\n", 0o755)
	write("promote.sh", "devctl release promote o/r\n", 0o644)
	write("heredoc.sh", "python3 - <<'EOF'\nimport os\nos.system('devctl pr merge o/r 7')\nEOF\n", 0o644)
	write("pieces.sh", "bash -c 'cd x; '\"devctl pr merge o/r 7\"\n", 0o644)
	script := filepath.Join(dir, "merge.sh")
	const line5 = "line 5: devctl pr merge o/r $pr | tee \"m-$pr.json\""
	for _, c := range []struct {
		cmd  string
		want []string
	}{
		// A script file, run by a shell, by its path, sourced, through a variable or another script.
		{"bash merge.sh", []string{"a devctl pr merge in the script " + script, line5, "or with the gate written in: " + g + "devctl pr merge o/r $pr | tee \"m-$pr.json\""}},
		{"zsh -x ./merge.sh 2>&1 | tail -3", []string{"the script " + script, line5}},
		{"./merge.sh", []string{"the script " + script, line5}},
		{"cd " + dir + " && source merge.sh", []string{"the script " + script, line5}},
		{"S=" + dir + "; bash \"$S/merge.sh\"", []string{"the script " + script, line5}},
		{"nohup bash outer.sh >| out.log 2>&1 &", []string{"the script " + script, line5}},
		{"bash promote.sh", []string{"a devctl release promote in the script " + filepath.Join(dir, "promote.sh"), "line 1: devctl release promote o/r"}},
		// A -c string in a script is read as the shell builds it, its pieces joined.
		{"bash pieces.sh", []string{"a devctl pr merge in the script " + filepath.Join(dir, "pieces.sh"), `line 1: bash -c 'cd x; '"devctl pr merge o/r 7"`}},
		{"bash heredoc.sh", []string{"a devctl pr merge in the here-document python3 reads", "line 2: os.system('devctl pr merge o/r 7')", "os.system('" + g + "devctl pr merge o/r 7')"}},
		// One written and run in the same call.
		{"cat <<'EOF' >| run.sh\ndevctl pr merge o/r 7\nEOF\nbash run.sh", []string{"this call, which runs run.sh (not readable before the call)", "line 2: devctl pr merge o/r 7"}},
		// An interpreter's script, by name or by its shebang.
		{"python3 merge.py", []string{"a devctl pr merge in the script " + filepath.Join(dir, "merge.py"), "line 3: subprocess.run([\"devctl\", \"pr\", \"merge\", \"o/r\", \"7\"], check=True)"}},
		{"./merge.py", []string{"the script " + filepath.Join(dir, "merge.py"), "line 3:"}},
		// A here-document or input an interpreter reads.
		{"python3 - <<'EOF'\nimport subprocess\nsubprocess.run([\"devctl\", \"pr\", \"merge\", \"o/r\", \"7\"])\nEOF", []string{"the here-document python3 reads", "line 2: subprocess.run(["}},
		{"echo 'devctl pr merge o/r 7' | bash", []string{"a devctl pr merge in the input bash reads", "line 1: echo 'devctl pr merge o/r 7'"}},
		{"cat merge.sh | sh", []string{"the script " + script, line5}},
		{"bash <<< 'devctl pr merge o/r 7'", []string{"the here-string bash reads", "line 1: devctl pr merge o/r 7", "or with the gate written in: " + g + "devctl pr merge o/r 7"}},
		{"sh < merge.sh", []string{"the script " + script, line5}},
		// An interpreter line.
		{"python3 -c 'import subprocess; subprocess.run([\"devctl\", \"pr\", \"merge\", \"o/r\", \"7\"])'", []string{"a devctl pr merge in python3's code (-c)", "line 1: import subprocess; subprocess.run([\"devctl\", \"pr\", \"merge\", \"o/r\", \"7\"])", "Run it as its own Bash command line."}},
		{"node -e 'require(\"child_process\").execSync(\"devctl pr merge o/r 7\")'", []string{"in node's code (-e)", "or with the gate written in: require(\"child_process\").execSync(\"" + g + "devctl pr merge o/r 7\")"}},
		{"perl -e 'system(\"devctl release promote o/r\")'", []string{"a devctl release promote in perl's code (-e)"}},
		{"ruby -e 'system(\"devctl pr wait o/r 7\")'", []string{"a devctl pr wait in ruby's code (-e)"}},
	} {
		d := decide(t, h, dir, c.cmd, nil)
		if d == nil || d.PermissionDecision != decisionDeny {
			t.Errorf("%q: want a refusal, got %+v", c.cmd, d)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(d.Reason, w) {
				t.Errorf("%q: reason lacks %q:\n%s", c.cmd, w, d.Reason)
			}
		}
	}
	for _, cmd := range []string{
		"bash gated.sh", "bash mention.sh", "./mention.sh", "cat merge.sh", "grep -n merge merge.sh", "bash /nowhere/merge.sh",
		"python3 -c 'print(\"devctl pr view o/r 7\")'", "python3 -m pytest tests/", "bash -n merge.sh && echo ok | bash",
		g + "devctl pr merge o/r 7 | tee m.json | ./mention.sh record o/r#7",
	} {
		if d := decide(t, h, dir, cmd, nil); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("%q is refused: %s", cmd, d.Reason)
		}
	}
}

// dataFiles writes the files of the data-file tests into dir: a log and a
// report whose text names the gated commands as a log and a report do, the
// read-only programs that read them, and the scripts that run a merge.
func dataFiles(t *testing.T, dir, gate string) {
	t.Helper()
	for name, content := range map[string]string{
		"log.txt":   "{\"at\": \"2026-10-11T12:47:42Z\", \"line\": \"devctl pr wait o/r 7\"}\ndevctl pr merge o/r 7\nbash merge.sh\n",
		"report.md": "BK 874: done. merging 7:\ndevctl pr merge o/r 7 ran through the gate; a bash merge.sh is refused, as is\nbash <<'EOF'\ndevctl release promote o/r\nEOF\n",
		"count.py":  "import sys\nprint(len(open(sys.argv[1]).read()))\n",
		"count.sh":  "grep -c merge \"$1\"\n",
		"merge.py":  "import subprocess, sys\nsubprocess.run([\"devctl\", \"pr\", \"merge\", \"o/r\", sys.argv[1]], check=True)\n",
		"merge.sh":  "set -e\ndevctl pr merge o/r \"$1\"\n",
		"gated.sh":  "set -e\n" + gate + "devctl pr merge o/r \"$1\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestHookLeavesADataFileUnread: the hook reads a file as a script only where
// the call runs it. A file the program only reads is data, whatever its text
// says: one after the program (the script, a -c or -e code, the - of a
// program on the input, however the options before it combine), a module's
// argument, a read-only command's argument, and a $(<file) or $(cat file) in
// a quoted argument of a command that only carries text (the class of
// https://github.com/giantswarm/beekeeper/issues/841, one level down:
// https://github.com/giantswarm/beekeeper/issues/964).
func TestHookLeavesADataFileUnread(t *testing.T) {
	h := Hook{Self: self, Shell: testShell, Clusters: func() []string { return nil }, Leases: func() []lease.Holder { return nil }}
	dir := t.TempDir()
	dataFiles(t, dir, self+" gate -- ")
	log := filepath.Join(dir, "log.txt")
	for _, cmd := range []string{
		// The acceptance criterion: a read-only one-liner on a data file.
		`python3 -c 'import sys; print(open(sys.argv[1]).read())' log.txt`,
		`python3 -I -c 'import sys; print(open(sys.argv[1]).read())' ` + log,
		`cd ` + dir + ` && python3 -X utf8 -c 'import sys; print(open(sys.argv[1]).read())' log.txt | tail -3`,
		// The live case: the program on the input, the file its argument.
		"f=" + log + "; python3 -I - \"$f\" <<'EOF'\nimport sys, io\np=sys.argv[1]; s=io.open(p, encoding='utf-8').read()\nprint(s)\nEOF",
		"python3 - log.txt <<'EOF'\nimport sys\nprint(open(sys.argv[1]).read())\nEOF",
		// A file after the script, whatever the options before the script.
		"python3 count.py log.txt",
		"python3 -I count.py " + log + " >| out.md && wc -c out.md",
		"python3 count.py -c log.txt",
		"bash count.sh log.txt",
		"bash -o pipefail count.sh log.txt",
		"bash gated.sh 7 log.txt",
		// A module's argument.
		"python3 -m json.tool log.txt",
		"python3 -m pytest tests/ log.txt",
		// The other interpreters' one-liners.
		`node -e 'console.log(require("fs").readFileSync(process.argv[1], "utf8"))' log.txt`,
		`node -r ./setup.js -e 'console.log(1)' log.txt`,
		`perl -ne 'print if /merge/' log.txt`,
		`perl -lane 'print $F[0]' log.txt`,
		`ruby -e 'puts File.read(ARGV[0])' log.txt`,
		`ruby -ne 'puts $_ if /merge/' log.txt`,
		`php -r 'echo file_get_contents($argv[1]);' log.txt`,
		// A read-only command's argument, before an interpreter's code too.
		"grep -n 'devctl pr merge' log.txt",
		"sed -n 2p log.txt | tail -1",
		"cat log.txt | python3 -c 'import sys; print(sys.stdin.read())'",
		"grep merge log.txt | python3 count.py /dev/stdin",
		// A file read into a quoted argument of a command that only carries text.
		`beekeeper agents idle --done --report "$(<report.md)" --problem none`,
		`R=` + dir + `; beekeeper agents idle --done --report "$(<$R/report.md)" --problem "the hook read the report as a script"`,
		`beekeeper agents idle --done --report "$(cat report.md)" --problem none`,
		`beekeeper agents idle --done --report "$(< ` + filepath.Join(dir, "report.md") + `)" --problem none`,
		`beekeeper note add --for x "$(<` + filepath.Join(dir, "report.md") + `)"`,
		// Wrapped in beekeeper run, the same.
		self + ` run -- zsh -c 'python3 -c "import sys; print(open(sys.argv[1]).read())" log.txt'`,
		self + ` run -- zsh -c 'python3 count.py log.txt'`,
	} {
		if d := decide(t, h, dir, cmd, nil); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("%q is refused: %s", cmd, d.Reason)
		}
	}
}

// TestHookGatesTheProgramItRuns: what the call runs is read as before, data
// files beside it or not: a script with a merge is refused, the refusal
// naming the file, the matched token and its line (so a data file the call
// does run as a program is seen as such), the forms https://github.com/giantswarm/beekeeper/pull/909
// gates in place stay gated in place, and a beekeeper run wrapper changes
// nothing of it.
func TestHookGatesTheProgramItRuns(t *testing.T) {
	h := Hook{Self: self, Shell: testShell, Clusters: func() []string { return nil }, Leases: func() []lease.Holder { return nil }}
	g := self + " gate -- "
	dir := t.TempDir()
	dataFiles(t, dir, g)
	log, script, py := filepath.Join(dir, "log.txt"), filepath.Join(dir, "merge.sh"), filepath.Join(dir, "merge.py")
	const merge, line2, logLine2 = "a devctl pr merge", "line 2:", "line 2: devctl pr merge o/r 7"
	for _, c := range []struct {
		cmd  string
		want []string // the file, the token and the line
	}{
		{"bash merge.sh 7 log.txt", []string{"the script " + script, merge, "line 2: devctl pr merge o/r \"$1\""}},
		{"bash -o pipefail merge.sh 7", []string{"the script " + script, merge, line2}},
		{"python3 merge.py 7 log.txt", []string{"the script " + py, merge, "line 2: subprocess.run("}},
		{"python3 -I merge.py 7", []string{"the script " + py, merge, line2}},
		{"python3 -X utf8 merge.py 7 -c log.txt", []string{"the script " + py, merge, line2}},
		{"php -f merge.py", []string{"the script " + py, merge, line2}},
		// A data file the call does run as its program: the refusal names it.
		{"bash log.txt", []string{"the script " + log, merge, logLine2}},
		{"sh < log.txt", []string{"the script " + log, merge, logLine2}},
		{"cat log.txt | bash", []string{"the script " + log, merge, logLine2}},
		{"python3 log.txt", []string{"the script " + log, "a devctl pr wait", "line 1: {\"at\": \"2026-10-11T12:47:42Z\", \"line\": \"devctl pr wait o/r 7\"}"}},
		{"python3 -I - < log.txt", []string{"the script " + log, "a devctl pr wait", "line 1:"}},
	} {
		d := decide(t, h, dir, c.cmd, nil)
		if d == nil || d.PermissionDecision != decisionDeny {
			t.Errorf("%q: want a refusal, got %+v", c.cmd, d)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(d.Reason, w) {
				t.Errorf("%q: reason lacks %q:\n%s", c.cmd, w, d.Reason)
			}
		}
	}
	for _, c := range []struct{ cmd, want string }{
		{"bash <<'EOF'\nset -e\ndevctl pr merge o/r 7\nEOF", "bash <<'EOF'\nset -e\n" + g + "devctl pr merge o/r 7\nEOF"},
		{"cat <<'EOF' | sh\ndevctl release promote o/r\nEOF", "cat <<'EOF' | sh\n" + g + "devctl release promote o/r\nEOF"},
		{"make && devctl pr merge o/r 7", self + " run -- zsh -c 'make && " + g + "devctl pr merge o/r 7'"},
	} {
		if d := decide(t, h, dir, c.cmd, nil); d == nil || d.UpdatedInput["command"] != c.want {
			t.Errorf("%q: got %+v, want %q", c.cmd, d, c.want)
		}
	}
}

// A quoted argument that a command may run as a command line keeps its gate:
// only one of a command that reads it as text stays as written.
func TestHookGatesQuotedCommandLines(t *testing.T) {
	h := Hook{Self: self, Shell: testShell, Clusters: func() []string { return nil }, Leases: func() []lease.Holder { return nil }}
	for _, cmd := range []string{
		"bash -o pipefail -c 'cd x; devctl pr merge o/r 7'",
		"bash --login -c 'cd x; devctl pr merge o/r 7'",
		"flock x.lock -c 'cd x; devctl pr merge o/r 7'",
		"flock --command 'cd x; devctl pr merge o/r 7' x.lock",
		"watch 'cd x; devctl pr wait o/r 7'",
		"su u -c 'cd x; devctl pr merge o/r 7'",
		"script -qc 'cd x; devctl pr merge o/r 7' /dev/null",
		"tmux new -d 'cd x; devctl pr merge o/r 7'",
		"echo 'cd x; devctl pr merge o/r 7' | sh",
		"printf '%s\\n' 'cd x; devctl pr merge o/r 7' | xargs -I{} sh -c '{}'",
		"git -c alias.m='!cd x; devctl pr merge o/r 7' m",
		"git rebase -x 'cd x; devctl pr merge o/r 7' main",
		"git submodule foreach 'cd x; devctl pr merge o/r 7'",
		"gh alias set m --shell 'cd x; devctl pr merge o/r 7'",
		"$(echo 'cd x; devctl pr merge o/r 7')",
		"`echo 'cd x; devctl pr merge o/r 7'`",
		"sh <(echo 'cd x; devctl pr merge o/r 7')",
		"echo 'cd x; devctl pr merge o/r 7' | python3 -c 'import os,sys; os.system(sys.stdin.read())'",
		"echo 'cd x; devctl pr merge o/r 7' | parallel",
		"echo 'cd x; devctl pr merge o/r 7' >(sh)",
		"{ echo 'cd x; devctl pr merge o/r 7'; } | sh",
		"for i in 1; do echo 'cd x; devctl pr merge o/r 7'; done | sh",
		"GIT_EDITOR=sh git commit -m 'cd x; devctl pr merge o/r 7'",
		"EDITOR=sh gh issue create -b 'cd x; devctl pr merge o/r 7'",
		"git grep -O'sh -c' 'cd x; devctl pr merge o/r 7'",
		"echo 'cd x; devctl pr merge o/r 7' | sort --compress-program=sh",
		"rg --pre sh 'cd x; devctl pr merge o/r 7' .",
		"export GIT_EDITOR=sh; git commit -e -m 'cd x; devctl pr merge o/r 7'",
		"git commit -e -m 'cd x; devctl pr merge o/r 7'",
		"printf -v c 'cd x; devctl pr merge o/r 7'; eval \"$c\"",
		"echo 'cd x; devctl pr merge o/r 7' >| m.sh; sh m.sh",
		"echo 'cd x; devctl pr merge o/r 7' | tee m.sh; sh m.sh",
		"gh issue create -we -b 'cd x; devctl pr merge o/r 7'",
		"gh issue create --edit=true -b 'cd x; devctl pr merge o/r 7'",
		"git show --output=m.sh -s --format='cd x; devctl pr merge o/r 7'; sh m.sh",
		"yq -i '.x = \"cd x; devctl pr merge o/r 7\"' m.yaml",
		"command export GIT_EDITOR=sh; git commit -m 'cd x; devctl pr merge o/r 7'",
		"env GIT_EDITOR=sh true; echo 'cd x; devctl pr merge o/r 7'",
		"git tag -a v1 'cd x; devctl pr merge o/r 7'",
		"gh --repo o/r issue create -b 'cd x; devctl pr merge o/r 7'",
		"hash -p /bin/sh gh; gh issue create -b 'cd x; devctl pr merge o/r 7'",
		">| m.sh; echo 'cd x; devctl pr merge o/r 7'",
		"env -S 'sh -c' echo 'cd x; devctl pr merge o/r 7'",
		"sudo -s echo 'cd x; devctl pr merge o/r 7'",
		"xargs -a args.txt echo 'cd x; devctl pr merge o/r 7'",
		"'sh -c' echo 'cd x; devctl pr merge o/r 7'",
		"/tmp/gh/echo 'cd x; devctl pr merge o/r 7'",
	} {
		d := decide(t, h, t.TempDir(), cmd, nil)
		if d == nil {
			t.Errorf("%q passes ungated", cmd)
			continue
		}
		if got, _ := d.UpdatedInput["command"].(string); d.PermissionDecision != decisionDeny && !strings.Contains(got, self+" gate ") {
			t.Errorf("%q passes ungated: %q", cmd, got)
		}
	}
}

// TestHookGatesRealMerges feeds every devctl pr merge statement from the
// transcripts of the machine the hook runs on, reduced to its shell skeleton
// (words outside the shell structure are x, repositories o/r), with the
// number of merges a shell parser finds in it. Each merge is gated, as is
// each blocking wait beside it, and the rewrite adds nothing but the gates.
// waits are the blocking waits in a command, mentions included.
var waits = regexp.MustCompile(`devctl\s+(?:pr|release|rollout)\s+wait\b`)

func TestHookGatesRealMerges(t *testing.T) {
	h := Hook{Self: self, Shell: testShell, Clusters: func() []string { return nil }, Leases: func() []lease.Holder { return nil }}
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
		// A mention inside a quoted argument (a grep pattern) stays as
		// written: more gates than merges only where there are mentions.
		k, want := strings.Count(got, self+" gate -- "), c.Merges+len(waits.FindAllString(c.Command, -1))
		if k < want || k > want && len(anyOwned.FindAllString(c.Command, -1)) == want {
			t.Errorf("%q: %d gates, want %d: %s", c.Command, k, want, got)
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
	h := Hook{Self: self, Shell: testShell, Clusters: func() []string { return nil }, Leases: func() []lease.Holder { return nil }}
	g := self + " gate -- "
	for _, c := range []struct {
		cmd, fixed string
		bg         bool
	}{
		{`bash -c 'devctl pr merge o/r 7' | tee m.json`, `bash -c '` + g + `devctl pr merge o/r 7' | tee m.json`, false},
		{`zsh -lc "flock x.lock ~/bin/devctl pr merge o/r 7 >| \"$S/m.json\""`, `zsh -lc "flock x.lock ` + g + `~/bin/devctl pr merge o/r 7 >| \"$S/m.json\""`, false},
		{`timeout 600 sh -c "devctl pr merge o/r 7"`, `timeout 600 sh -c "` + self + ` gate --wait 30m --limit 0 -- devctl pr merge o/r 7"`, true},
		{self + ` run -- zsh -c 'devctl pr merge o/r 7'`, self + ` run -- zsh -c '` + g + `devctl pr merge o/r 7'`, false},
		{`bash -c 'devctl pr wait o/r 7'`, `bash -c '` + g + `devctl pr wait o/r 7'`, false},
		{`eval "devctl pr merge o/r 7"`, `eval "` + g + `devctl pr merge o/r 7"`, false},
		// Adjacent quoted and bare pieces are one -c string, as the shell joins them.
		{`bash -c 'cd x; '"devctl pr merge o/r 7"`, `bash -c 'cd x; '"` + g + `devctl pr merge o/r 7"`, false},
		{`sh -c "cd x; "'devctl pr merge o/r 7'`, `sh -c "cd x; "'` + g + `devctl pr merge o/r 7'`, false},
		{`zsh -c 'cd x; '$'devctl release promote o/r'`, `zsh -c 'cd x; '$'` + g + `devctl release promote o/r'`, false},
		{`bash -c 'cd x; 'devctl\ pr\ merge\ o/r\ 7`, `bash -c 'cd x; '` + strings.ReplaceAll(g, " ", `\ `) + `devctl\ pr\ merge\ o/r\ 7`, false},
		{`bash -c 'devctl pr '"merge o/r 7" | tee m.json`, `bash -c '` + g + `devctl pr '"merge o/r 7" | tee m.json`, false},
		{`bash -c "cd \"$D\"; "'devctl'" pr wait o/r 7"`, `bash -c "cd \"$D\"; "'` + g + `devctl'" pr wait o/r 7"`, false},
		{`bash -c devctl\ pr\ merge\ o/r\ 7`, `bash -c ` + strings.ReplaceAll(g, " ", `\ `) + `devctl\ pr\ merge\ o/r\ 7`, false},
		// $'…' escapes are decoded as bash decodes them.
		{`bash -c $'\x64evctl pr merge o/r 7'`, `bash -c $'` + g + `\x64evctl pr merge o/r 7'`, false},
		{`bash -c 'cd x;'$'\n\144evctl pr merge o/r 7'`, `bash -c 'cd x;'$'\n` + g + `\144evctl pr merge o/r 7'`, false},
		// eval runs all its words, joined by spaces.
		{`eval 'cd x;' devctl pr merge o/r 7`, `eval 'cd x;' ` + strings.ReplaceAll(g, " ", `\ `) + `devctl pr merge o/r 7`, false},
		// A -c string the parser cannot resolve fails closed: the rest of the line is its text.
		{`bash -c "$(printf 'cd x; ')"'devctl pr merge o/r 7'`, `bash -c "$(printf 'cd x; ')"'` + g + `devctl pr merge o/r 7'`, false},
		{"bash -c 'cd x; '\"`echo`devctl pr merge o/r 7\"", "bash -c 'cd x; '\"`echo`" + g + "devctl pr merge o/r 7\"", false},
		{`bash -c 'cd x; '"devctl pr merge o/r 7`, `bash -c 'cd x; '"` + g + `devctl pr merge o/r 7`, false},
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
	for _, cmd := range []string{`bash -c 'devctl pr view o/r 7'`, `bash -c 'echo ok' && echo "devctl pr merge o/r 7"`, `ssh -c aes x`,
		// Words after a -c string are its arguments, not its text; a joined string without a merge runs none.
		`bash -c 'echo "$@"' _ devctl pr merge o/r 7`, `bash -c 'cd x; '"devctl pr view o/r 7"`, `bash -c "$(cat run.sh)"`,
		`bash -c $'echo \'devctl\tpr view\''`} {
		if d := decide(t, h, t.TempDir(), cmd, nil); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("%q is refused: %s", cmd, d.Reason)
		}
	}
	// A string the parser cannot resolve that names devctl ungated fails closed.
	for _, cmd := range []string{`bash -c "$(echo devctl) pr merge o/r 7"`, "sh -c \"`printf devctl` release promote o/r\"",
		`bash -c 'x=1; '"$(printf '%s' ~/bin/devctl) pr wait o/r 7"`} {
		if d := decide(t, h, t.TempDir(), cmd, nil); d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "cannot read") {
			t.Errorf("%q: got %+v, want a refusal", cmd, d)
		}
	}
}

// A configuration that does not load refuses every Bash call with its error.
func TestBrokenConfigRefusesBash(t *testing.T) {
	h := hook()
	h.ConfigErr = errors.New("config.yaml: line 3: bad indentation")
	d := decide(t, h, "/", "git status", nil)
	if d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "line 3: bad indentation") {
		t.Errorf("a broken configuration passes: %+v", d)
	}
}

// A beekeeper secret call on the vault may wait for the person's unlock:
// it gets the Bash tool's 10 minutes, its command unchanged.
func TestVaultCallGetsTheLongTimeout(t *testing.T) {
	for cmd, long := range map[string]bool{
		"beekeeper secret fingerprint op://Shared/i/f":                     true,
		"cd x && beekeeper secret copy op://Shared/i/f a.sops.yaml#data.t": true,
		"beekeeper secret compare a.sops.yaml b.sops.yaml":                 false,
	} {
		d := decide(t, hook(), "/", cmd, map[string]any{"timeout": 120000})
		switch {
		case long && (d == nil || d.UpdatedInput["timeout"] != float64(600000) || d.UpdatedInput["command"] != cmd):
			t.Errorf("%q: %+v, want the command with the 10 minutes", cmd, d)
		case !long && d != nil && d.UpdatedInput["timeout"] == float64(600000):
			t.Errorf("%q: raised the timeout", cmd)
		}
	}
}

// ansiC decodes $'…' escapes as bash does; a control byte after one is no
// digit (0x14 is no "4").
func TestANSICEscapes(t *testing.T) {
	for in, want := range map[string]string{`\x64`: "d", `\144`: "d", `\u0064`: "d", `\U00000064`: "d", `\n`: "\n", `\cA`: "\x01",
		`\x6G`: "\x06", "\\1\x14": "\x01", "\\x\x16": `\x`, `\q`: `\q`} {
		b, n := ansiC(in)
		if string(b) != want {
			t.Errorf("ansiC(%q) = %q, %d; want %q", in, b, n, want)
		}
	}
}
