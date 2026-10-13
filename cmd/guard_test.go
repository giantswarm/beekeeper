package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/free"
	"github.com/giantswarm/beekeeper/internal/gocache"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The test binary doubles as beekeeper (BEEKEEPER_TEST_MAIN=1), so the hook's
// rewrite, which names the running binary, runs this build's `run`; and as a
// memory hog ("__alloc") for the cap.
func TestMain(m *testing.M) {
	// No test frees the machine's /tmp, nor the test binary run as
	// beekeeper: the automatic free reads no process table, so it frees
	// nothing; a test that wants a run gives it a machine.
	liveTmpfs = func(*app) (free.Machine, error) {
		return free.Machine{TableErr: errors.New("no machine in a test")}, nil
	}
	// Nor its Go build cache: the trim sees only a cache a test points
	// GOCACHE at; the cache the tests started with is no cache to it.
	machineCache := gocache.Dir()
	goCacheDir = func() string {
		if d := gocache.Dir(); d != machineCache {
			return d
		}
		return ""
	}
	// The test binary is the askpass helper too, as the beekeeper binary
	// is: the helper copy --git-askpass writes runs this executable.
	if len(os.Args) > len(secret.AskpassArgs) && slices.Equal(os.Args[1:1+len(secret.AskpassArgs)], secret.AskpassArgs) {
		os.Exit(Main())
	}
	if len(os.Args) > 1 && os.Args[1] == "__alloc" {
		b := make([]byte, 512<<20)
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1
		}
		os.Exit(int(b[0]) - 1)
	}
	// A run that records its owner's wakes runs merge-child whole, its
	// outcome handed over (tellOwner) into the file instead of a wake.
	if wakes := os.Getenv(testWakesEnv); wakes != "" {
		wakeOwner = recordWakes(wakes)
	} else if len(os.Args) > 2 && os.Args[1] == mergeChildCmd {
		os.Exit(mergeChild(os.Args[2]).rc)
	}
	if len(os.Args) > 2 && os.Args[1] == followRunCmd {
		os.Exit(followRun(os.Args[2], os.Stdout, os.Stderr))
	}
	if os.Getenv("BEEKEEPER_TEST_MAIN") == "1" {
		os.Exit(Main())
	}
	// A test inside the agent sandbox would inherit its markers and ask a
	// broker it never started; a test that wants them sets them itself.
	for _, k := range []string{sandbox.Env, sandbox.Brokered} {
		if err := os.Unsetenv(k); err != nil {
			panic(err)
		}
	}
	// No test asks the machine's Claude Code for its plugins; one that
	// wants a list sets it.
	listPlugins = func(context.Context) ([]installedPlugin, error) { return nil, nil }
	os.Exit(m.Run())
}

// testWakesEnv names the file a test binary run as beekeeper records its
// owners' wakes in, one line each, instead of waking anyone (TestMain).
const testWakesEnv = "BEEKEEPER_TEST_WAKES"

// recordWakes is wakeOwner recording each wake in the file at path.
func recordWakes(path string) func(*app, context.Context, state.Party, string, string, string) error {
	return func(_ *app, _ context.Context, by state.Party, q, msg, _ string) error {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // the test's file
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = f.WriteString(by.Name + " → " + q + ": " + msg + "\n")
		return err
	}
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
	if _, err := exec.LookPath(testShell); err != nil {
		t.Skip("no zsh")
	}
	if !plat.Launcher.Available() {
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
	c := exec.Command("systemd-run", "--user", "--scope", "--quiet", "--expand-environment=no", "--", testShell, "-c", command) //nolint:gosec // the test's own commands
	c.Env = env
	var o, e strings.Builder
	c.Stdout, c.Stderr = &o, &e
	_ = c.Run()
	return o.String(), e.String(), c.ProcessState.ExitCode()
}

// testShell is the shell the tests' hook rewrites into.
const testShell = "zsh"

func rewrite(t *testing.T, command string) string {
	t.Helper()
	return rewriteCall(t, command, false)
}

// rewriteCall is the command the hook runs for a Bash call, a background
// one when bg: the command itself when the hook leaves it unchanged.
func rewriteCall(t *testing.T, command string, bg bool) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": command, "run_in_background": bg}})
	var o struct {
		D struct {
			UpdatedInput struct{ Command string } `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	out := guard.Hook{Self: self, Shell: testShell}.Decide(raw)
	if out == nil {
		return command
	}
	if err := json.Unmarshal(out, &o); err != nil {
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

// TestRunHoldsASlotInACappedScope: the holder record names the build that
// holds the slot, here behind a command too long for the record's line.
func TestRunHoldsASlotInACappedScope(t *testing.T) {
	state, env := capped(t)
	out, errOut, rc := zsh(t, env, rewrite(t, `grep -o 'memcap.slice/memcap-slot1_test.slice/memcap-test-[0-9]*-[0-9]*' /proc/self/cgroup; cat "$MEMCAP_STATE/slots/1.holder"; if false; then go test; fi`))
	if rc != 0 || !strings.HasPrefix(out, "memcap.slice/memcap-slot1_test.slice/"+guard.TestScopePrefix) {
		t.Fatalf("rc %d, stdout %q, stderr %q", rc, out, errOut)
	}
	var h struct {
		PID        int
		Cmd, Max   string
		CWD, Since string
	}
	if _, rec, _ := strings.Cut(out, "\n"); json.Unmarshal([]byte(rec), &h) != nil || h.PID == 0 || h.Max != "64M" || h.Cmd != "… go test; fi" {
		t.Errorf("holder record %q", out)
	}
	if _, err := os.Stat(filepath.Join(state, "slots", "1.holder")); !os.IsNotExist(err) {
		t.Errorf("holder record left behind: %v", err)
	}
}

// holdSlots holds both of the state's slots until the test ends.
func holdSlots(t *testing.T, state string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(state, "slots"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, i := range []string{"1", "2"} {
		l := flock.New(filepath.Join(state, "slots", i+".lock"))
		if ok, err := l.TryLock(); !ok || err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Unlock() })
		_ = os.WriteFile(filepath.Join(state, "slots", i+".holder"), []byte(`{"pid":1,"cmd":"go test holder-`+i+`"}`), 0o600)
	}
}

// The exit-75 message states the wait applied and where it came from; only
// a foreground run is advised to run in the background.
func TestRunExits75WhenEverySlotIsHeld(t *testing.T) {
	state, env := capped(t)
	holdSlots(t, state)
	self, _ := os.Executable()
	_, errOut, rc := zsh(t, env, self+" run --wait 1s -- true")
	if rc != guard.ExitBusy || !strings.Contains(errOut, "slot 2: {\"pid\":1,\"cmd\":\"go test holder-2\"}") ||
		!strings.Contains(errOut, guard.LogPrefix+"gave up after 1s (--wait) waiting for") ||
		!strings.Contains(errOut, "run_in_background (the wait is then 60m)") {
		t.Errorf("foreground: rc %d, stderr %q", rc, errOut)
	}
	_, errOut, rc = zsh(t, append(env, "MEMCAP_WAIT=1s"), "export MEMCAP_BACKGROUND=1; "+self+" run -- true")
	if rc != guard.ExitBusy || !strings.Contains(errOut, guard.LogPrefix+"gave up after 1s (MEMCAP_WAIT) waiting for") ||
		strings.Contains(errOut, "run_in_background") {
		t.Errorf("background: rc %d, stderr %q", rc, errOut)
	}
}

// A beekeeper run a background Bash call reaches directly, after a cd and
// export chain or through a wrapper script waits the background default;
// timeout cuts the wait once it has started.
func TestBackgroundCallsRunWaitTheBackgroundDefault(t *testing.T) {
	state, env := capped(t)
	env = slices.DeleteFunc(env, func(e string) bool { return strings.HasPrefix(e, "MEMCAP_WAIT=") })
	holdSlots(t, state)
	self, _ := os.Executable()
	dir := t.TempDir()
	script := filepath.Join(dir, "build.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+guard.ShellQuote(self)+" run -- true\n"), 0o700); err != nil { //nolint:gosec // an executable script
		t.Fatal(err)
	}
	for shape, cmd := range map[string]string{
		"direct": self + " run -- true",
		"chain":  "cd " + guard.ShellQuote(dir) + " && export FOO=1 && " + self + " run -- true",
		"script": script,
	} {
		_, errOut, rc := zsh(t, env, "timeout 3 "+testShell+" -c "+guard.ShellQuote(rewriteCall(t, cmd, true)))
		if rc != 124 || !strings.Contains(errOut, guard.LogPrefix+"waiting up to 60m (the background default) for a build slot") {
			t.Errorf("%s: rc %d, stderr %q", shape, rc, errOut)
		}
		_, errOut, rc = zsh(t, append(env, "MEMCAP_WAIT=1s"), rewriteCall(t, cmd, false))
		if rc != guard.ExitBusy || !strings.Contains(errOut, "gave up after 1s (MEMCAP_WAIT)") {
			t.Errorf("%s in the foreground: rc %d, stderr %q", shape, rc, errOut)
		}
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
	kills, err := plat.Machine.OOMKills(context.Background(), start.Add(-time.Second))
	if err != nil {
		t.Skipf("no kernel journal: %v", err)
	}
	store, _ := stateStore(dir)
	runs := &runIndex{store: store}
	for _, k := range kills {
		if strings.HasSuffix(k.RunMemcg(), "/"+guard.RunScope(evs[0].Detail)) {
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

func stateStore(dir string) (*state.FileStore, error) {
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
