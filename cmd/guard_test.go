package cmd

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The test binary doubles as beekeeper (BEEKEEPER_TEST_MAIN=1), so the hook's
// rewrite, which names the running binary, runs this build's `run`; and as a
// memory hog ("__alloc") for the cap.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__alloc" {
		b := make([]byte, 512<<20)
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1
		}
		os.Exit(int(b[0]) - 1)
	}
	if os.Getenv("BEEKEEPER_TEST_MAIN") == "1" {
		os.Exit(Main())
	}
	os.Exit(m.Run())
}

// Every kind of expansion the wrapper must not touch: zsh-only (${=x}, ${(f)y},
// ${(U)x}, pipestatus), POSIX braces on shell variables, defaults, $$, $#; the
// heavy marker at the end never runs, it only makes the hook wrap the command.
const shellSemantics = `x="a b"; y=$'l1\nl2'; e=""
printf '[%s]\n' ${=x}
printf '<%s>\n' "${x}" "${e:-empty}" "${#x}"
printf '{%s}\n' ${(f)y} ${(U)x}
true | false; printf 'ps=%s pid_ok=%s args=%d\n' "${pipestatus[1]}" "$(( $$ > 0 ))" $#
printf 'e=[%s]\n' ${=e}
if false; then go test ./...; fi`

const expected = "[a]\n[b]\n<a b>\n<empty>\n<3>\n{l1}\n{l2}\n{A B}\nps=0 pid_ok=1 args=0\ne=[]\n"

// capped skips without the user systemd and zsh a capped run needs, and
// returns the environment of a test's run (MEMCAP_TEST) with private slots
// and a 64M cap, so the test neither waits for the machine's build slots nor
// counts as a build, and its cap kill reads as a test kill.
func capped(t *testing.T) (state string, env []string) {
	t.Helper()
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("no zsh")
	}
	if !guard.UserSystemd() {
		t.Skip("no user systemd")
	}
	state = t.TempDir()
	return state, append(os.Environ(), "BEEKEEPER_TEST_MAIN=1", "BEEKEEPER_CONFIG="+filepath.Join(state, "none.yaml"),
		"XDG_STATE_HOME="+state, "MEMCAP_STATE="+state, "MEMCAP_MAX=64M", "MEMCAP_WAIT=30s", "MEMCAP_TEST=1",
		"CLAUDE_CODE_SESSION_ID=test-session", "CLAUDE_CODE_HOST_SESSION_ID=", "CLAUDE_CODE_SESSION_NAME=capped test")
}

// zsh runs the command the way the Bash tool does, in a scope of its own:
// `go test` itself runs in a capped scope on a guarded machine, and run
// inside one does not take a slot.
func zsh(t *testing.T, env []string, command string) (stdout, stderr string, rc int) {
	t.Helper()
	c := exec.Command("systemd-run", "--user", "--scope", "--quiet", "--expand-environment=no", "--", "zsh", "-c", command) //nolint:gosec // the test's own commands
	c.Env = env
	var o, e strings.Builder
	c.Stdout, c.Stderr = &o, &e
	_ = c.Run()
	return o.String(), e.String(), c.ProcessState.ExitCode()
}

func rewrite(t *testing.T, command string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": command}})
	var o struct {
		D struct {
			UpdatedInput struct{ Command string } `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(guard.Hook{Self: self}.Decide(raw), &o); err != nil {
		t.Fatal(err)
	}
	return o.D.UpdatedInput.Command
}

func TestRunKeepsTheCallersShellSemantics(t *testing.T) {
	_, env := capped(t)
	out, errOut, rc := zsh(t, env, rewrite(t, shellSemantics))
	if want, _, _ := zsh(t, os.Environ(), shellSemantics); want != expected {
		t.Fatalf("unwrapped: %q", want)
	}
	if strings.Contains(errOut, "evaluates to an empty string") || out != expected || rc != 0 {
		t.Errorf("wrapped: rc %d, stdout %q, stderr %q", rc, out, errOut)
	}
}

func TestRunHoldsASlotInACappedScope(t *testing.T) {
	state, env := capped(t)
	out, errOut, rc := zsh(t, env, rewrite(t, `grep -o 'memcap.slice/memcap-test-[0-9]*-[0-9]*' /proc/self/cgroup; cat "$MEMCAP_STATE/slots/1.holder"; if false; then go test; fi`))
	if rc != 0 || !strings.HasPrefix(out, "memcap.slice/"+guard.TestScopePrefix) {
		t.Fatalf("rc %d, stdout %q, stderr %q", rc, out, errOut)
	}
	var h struct {
		PID        int
		Cmd, Max   string
		CWD, Since string
	}
	if _, rec, _ := strings.Cut(out, "\n"); json.Unmarshal([]byte(rec), &h) != nil || h.PID == 0 || h.Max != "64M" || !strings.HasPrefix(h.Cmd, "zsh -c ") {
		t.Errorf("holder record %q", out)
	}
	if _, err := os.Stat(filepath.Join(state, "slots", "1.holder")); !os.IsNotExist(err) {
		t.Errorf("holder record left behind: %v", err)
	}
}

func TestRunExits75WhenEverySlotIsHeld(t *testing.T) {
	state, env := capped(t)
	if err := os.MkdirAll(filepath.Join(state, "slots"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, i := range []string{"1", "2"} {
		l := flock.New(filepath.Join(state, "slots", i+".lock"))
		if ok, err := l.TryLock(); !ok || err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Unlock() }()
		_ = os.WriteFile(filepath.Join(state, "slots", i+".holder"), []byte(`{"pid":1,"cmd":"go test holder-`+i+`"}`), 0o600)
	}
	self, _ := os.Executable()
	_, errOut, rc := zsh(t, env, self+" run --wait 1s -- true")
	if rc != guard.ExitBusy || !strings.Contains(errOut, "slot 2: {\"pid\":1,\"cmd\":\"go test holder-2\"}") || !strings.Contains(errOut, guard.LogPrefix+"gave up after 1s") {
		t.Errorf("rc %d, stderr %q", rc, errOut)
	}
}

// The kill is found again after the run has ended, as snapshot and watch
// find it, and reads as the test's own kill, never as a build's cap.
func TestRunNamesTheCapsVictim(t *testing.T) {
	dir, env := capped(t)
	self, _ := os.Executable()
	start := time.Now()
	_, errOut, rc := zsh(t, env, self+" run -- "+self+" __alloc")
	if rc != 137 || !strings.Contains(errOut, guard.LogPrefix+"the 64M cap killed: ") || !strings.Contains(errOut, "(exit 137) — bound the parallelism") {
		t.Errorf("rc %d, stderr %q", rc, errOut)
	}
	evs := runEvents(t, dir)
	if len(evs) != 2 || evs[0].Verb != guard.VerbStart || evs[1].Verb != guard.VerbEnd ||
		!strings.Contains(evs[1].Detail, "exit 137") || !strings.Contains(evs[1].Detail, "the 64M cap killed") {
		t.Fatalf("events %+v", evs)
	}
	kills, err := machine.OOMKills(context.Background(), start.Add(-time.Second))
	if err != nil {
		t.Skipf("no kernel journal: %v", err)
	}
	store, _ := stateStore(dir)
	runs := &runIndex{store: store}
	for _, k := range kills {
		if strings.HasSuffix(k.Memcg, "/"+guard.RunScope(evs[0].Detail)) {
			if got := oomOwner(k, nil, nil, &proc.Table{ByPID: map[int]*proc.Process{}}, runs); got != testKillOwner {
				t.Errorf("owner %q", got)
			}
			return
		}
	}
	t.Errorf("no kill in %s among %+v", guard.RunScope(evs[0].Detail), kills)
}

func TestRunRecordsItsStartAndEnd(t *testing.T) {
	dir, env := capped(t)
	if _, errOut, rc := zsh(t, env, rewrite(t, `echo  built;  if false; then go test ./...; fi`)); rc != 0 {
		t.Fatalf("rc %d, stderr %q", rc, errOut)
	}
	evs := runEvents(t, dir)
	if len(evs) != 2 {
		t.Fatalf("events %+v", evs)
	}
	scope := guard.RunScope(evs[0].Detail)
	for i, verb := range []string{guard.VerbStart, guard.VerbEnd} {
		e := evs[i]
		if e.Verb != verb || e.By.Session != "test-session" || e.By.Name != "capped test" || guard.RunScope(e.Detail) != scope ||
			!guard.IsTestScope(scope) || !strings.HasSuffix(scope, ".scope") ||
			guard.RunCommand(e.Detail) != "echo built; if false; then go test ./...; fi" {
			t.Errorf("event %d: %+v", i, e)
		}
	}
	if !strings.Contains(evs[1].Detail, " exit 0 after ") {
		t.Errorf("end %q", evs[1].Detail)
	}
}

func stateStore(dir string) (*state.Store, error) {
	return state.Open(filepath.Join(dir, "beekeeper"))
}

func runEvents(t *testing.T, dir string) []state.Event {
	t.Helper()
	s, err := stateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := s.Events(0, isRun)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}
