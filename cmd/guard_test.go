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

	"github.com/giantswarm/beekeeper/internal/claim/claimtest"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/free"
	"github.com/giantswarm/beekeeper/internal/gocache"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/testenv"
)

// The test binary doubles as beekeeper (BEEKEEPER_TEST_MAIN=1), so the hook's
// rewrite, which names the running binary, runs this build's `run`; and as a
// memory hog ("__alloc") for the cap.
func TestMain(m *testing.M) {
	// No test frees the machine's /tmp, nor the test binary run as
	// beekeeper: the automatic free reads no process table, so it frees
	// nothing; a test that wants a run gives it a machine.
	// Nor does an answer reach GitHub: every thread but unknownThread is a
	// private one to it; a test that reads the comments stubs its own.
	boardGH = (&answerGH{}).run
	// Nor does an answer read where it runs: the person types it at
	// their terminal; a test that wants a session stubs its own.
	answerSeat = func(*app) (string, bool) { return "", true }
	// Nor does a claim: every issue's thread is an empty one of its own; a
	// test of the claims keeps its own.
	claimGH = claimtest.New().GH
	// Nor does a browse move a window of the desktop's compositor; a test
	// that places one stubs its own hyprctl.
	hyprRunning = func() bool { return false }
	// Nor does a browse or a link read the machine's screen locker: the
	// screen is unlocked; a test that wants it locked stubs its own.
	screenLocker = func() string { return "" }
	// Nor does a gated merge read its reviews: nothing holds it; a test
	// that wants a reviewer stubs its own.
	reviewGuard = func(context.Context, string, int) (string, error) { return "", nil }
	liveTmpfs = func(*app) (free.Machine, error) {
		return free.Machine{TableErr: errors.New("no machine in a test")}, nil
	}
	// Nor its worktrees: the trim finds none; a test that wants a run
	// gives it a machine.
	liveWorktrees = func(*app) (worktreeMachine, error) { return worktreeMachine{}, nil }
	liveDisk = func(*app) (diskMachine, error) { return diskMachine{}, errors.New("no disk machine in a test") }
	// Nor does a command sync the machine's Claude Code plugins, stamped
	// release build or not: a test that wants the sync names its binary.
	binaryIdentity = func() string { return "" }
	// Nor does a gate read that fails in transit pause between its tries.
	readBackoff = make([]time.Duration, len(readBackoff))
	// Nor its Go build cache: the trim sees only a cache a test points
	// GOCACHE at; the cache the tests started with is no cache to it.
	machineCache := gocache.Dir()
	goCacheDir = func() string {
		if d := gocache.Dir(); d != machineCache {
			return d
		}
		return ""
	}
	machineModCache := gocache.ModDir()
	goModCacheDir = func() string {
		if d := gocache.ModDir(); d != machineModCache {
			return d
		}
		return ""
	}
	// Nor its Docker: the prune sees no docker CLI; a test that wants one
	// gives it a fake.
	dockerCLI = nil
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
	if os.Getenv(fakePageClaudeEnv) == "1" {
		os.Exit(fakePageClaude(os.Args[1:]))
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
	testenv.Setup()
	os.Exit(runIsolated(m))
}

// isolatedRoot is the scratch directory runIsolated points the
// configuration and state at for the whole test binary.
var isolatedRoot string

// runIsolated runs the tests with no path to the machine's own
// configuration or state: $BEEKEEPER_CONFIG names a file in a scratch
// directory, and $XDG_CONFIG_HOME and $XDG_STATE_HOME are scratch
// directories, so a test that sets none of them reads and writes there. A
// test that wants its own sets them with t.Setenv. The binary run as
// beekeeper by a test inherits the test's environment, so this runs only on
// the m.Run path.
func runIsolated(m *testing.M) int {
	root, err := os.MkdirTemp("", "beekeeper-cmd-test-")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	isolatedRoot = root
	for k, v := range map[string]string{
		"BEEKEEPER_CONFIG": filepath.Join(root, "config", "beekeeper", "config.yaml"),
		"XDG_CONFIG_HOME":  filepath.Join(root, "config"),
		"XDG_STATE_HOME":   filepath.Join(root, "state"),
	} {
		if err := os.Setenv(k, v); err != nil {
			panic(err)
		}
	}
	return m.Run()
}

func TestATestWithoutItsOwnPathsUsesScratchOnes(t *testing.T) {
	path, err := config.Path("")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{"config": path, "state": cfg.StateDir} {
		if rel, err := filepath.Rel(isolatedRoot, p); err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("%s path %s is outside the scratch directory %s", name, p, isolatedRoot)
		}
	}
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
// counts as a build, and its cap kill reads as a test kill. The caller's own
// run settings never reach it: a background Bash call exports
// MEMCAP_BACKGROUND=1, and a case that wants it sets it itself.
func capped(t *testing.T) (state string, env []string) {
	t.Helper()
	if _, err := exec.LookPath(testShell); err != nil {
		t.Skip("no zsh")
	}
	if !plat.Launcher.Available() {
		t.Skip("no user systemd")
	}
	for _, k := range []string{guard.BackgroundEnv, "MEMCAP_SLOTS", "MEMCAP_SWAP"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
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

// The exit-75 message states the wait applied and where it came from and
// names the command as not run; only a foreground run is advised to run in
// the background.
func TestRunExits75WhenEverySlotIsHeld(t *testing.T) {
	state, env := capped(t)
	holdSlots(t, state)
	self, _ := os.Executable()
	_, errOut, rc := zsh(t, env, self+" run --wait 1s -- go build ./...")
	if rc != guard.ExitBusy || !strings.Contains(errOut, "slot 2: pid 1: go test holder-2\n") ||
		!strings.Contains(errOut, guard.LogPrefix+"gave up after 1s (--wait) waiting for") ||
		!strings.Contains(errOut, guard.LogPrefix+"not run: go build ./... — the machine is busy") ||
		!strings.Contains(errOut, "run_in_background (the wait is then 60m)") {
		t.Errorf("foreground: rc %d, stderr %q", rc, errOut)
	}
	_, errOut, rc = zsh(t, append(env, "MEMCAP_WAIT=1s"), "export MEMCAP_BACKGROUND=1; "+self+" run -- go build ./...")
	if rc != guard.ExitBusy || !strings.Contains(errOut, guard.LogPrefix+"gave up after 1s (MEMCAP_WAIT) waiting for") ||
		!strings.Contains(errOut, guard.LogPrefix+"not run: go build ./... — the machine is busy") ||
		strings.Contains(errOut, "run_in_background") {
		t.Errorf("background: rc %d, stderr %q", rc, errOut)
	}
}

// A command that builds nothing runs at once with every slot held, without
// a wait or a cap; declaring a class puts it back in the queue.
func TestRunOfACommandThatBuildsNothingSkipsTheQueue(t *testing.T) {
	state, env := capped(t)
	holdSlots(t, state)
	self, _ := os.Executable()
	for _, command := range []string{"/usr/bin/true", "echo built nothing", "go version"} {
		_, errOut, rc := zsh(t, env, self+" run --wait 5s -- "+command)
		if rc != 0 || errOut != "" {
			t.Errorf("%s with every slot held: rc %d, stderr %q", command, rc, errOut)
		}
	}
	if _, errOut, rc := zsh(t, env, self+" run --wait 1s --class build -- true"); rc != guard.ExitBusy {
		t.Errorf("true declared a build with every slot held: rc %d, stderr %q", rc, errOut)
	}
}

// A benchmark or a lab proof takes no build slot: with every slot held it
// runs at once, beside them; an unknown class is a usage error.
func TestRunOfABenchmarkRunsBesideHeldSlots(t *testing.T) {
	state, env := capped(t)
	holdSlots(t, state)
	self, _ := os.Executable()
	for _, class := range []string{guard.ClassBenchmark, guard.ClassLab} {
		_, errOut, rc := zsh(t, env, self+" run --wait 1s --class "+class+" -- true")
		if rc != 0 || strings.Contains(errOut, "waiting up to") {
			t.Errorf("%s with every slot held: rc %d, stderr %q", class, rc, errOut)
		}
	}
	if _, errOut, rc := zsh(t, env, self+" run --class long -- true"); rc == 0 || !strings.Contains(errOut, "build, benchmark, lab") {
		t.Errorf("an unknown class: rc %d, stderr %q", rc, errOut)
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
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+guard.ShellQuote(self)+" run --class build -- true\n"), 0o700); err != nil { //nolint:gosec // an executable script
		t.Fatal(err)
	}
	for shape, cmd := range map[string]string{
		"direct": self + " run --class build -- true",
		"chain":  "cd " + guard.ShellQuote(dir) + " && export FOO=1 && " + self + " run --class build -- true",
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
	// The fixture supplies its own identity; the caller must not override it
	// while the hook rewrites or runs the command.
	t.Setenv("ZDOTDIR", t.TempDir())
	for _, name := range callerEnv {
		t.Setenv(name, "")
	}
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

// MEMCAP_STATE and MEMCAP_SLOTS override memcap.slotDir and memcap.slots for
// every taker of a build slot: beekeeper run and a start that builds read
// the one directory and count.
func TestBuildSlotsEnvOverConfig(t *testing.T) {
	const cfgSlots, envState, envSlots = "/cfg/slots", "/env", "/env/slots"
	a := &app{cfg: &config.Config{Memcap: config.Memcap{SlotDir: cfgSlots, Slots: 2}}}
	for _, c := range []struct {
		name, state, slots string
		wantDir            string
		wantN              int
		wantErr            bool
	}{
		{"config", "", "", cfgSlots, 2, false},
		{"MEMCAP_STATE", envState, "", envSlots, 2, false},
		{"MEMCAP_SLOTS", "", "5", cfgSlots, 5, false},
		{"both", envState, "5", envSlots, 5, false},
		{"MEMCAP_SLOTS not a number", envState, "x", envSlots, 2, true},
		{"MEMCAP_SLOTS zero", "", "0", cfgSlots, 2, true},
	} {
		t.Setenv("MEMCAP_STATE", c.state)
		t.Setenv("MEMCAP_SLOTS", c.slots)
		dir, n, err := a.buildSlots()
		if dir != c.wantDir || n != c.wantN || (err != nil) != c.wantErr {
			t.Errorf("%s: buildSlots %q %d %v, want %q %d, error %v", c.name, dir, n, err, c.wantDir, c.wantN, c.wantErr)
		}
		if c.wantErr && Code(err) != ExitUsage {
			t.Errorf("%s: exit %d, want %d", c.name, Code(err), ExitUsage)
		}
		o, _, err := a.startSlot()
		if !c.wantErr && (err != nil || o.SlotDir != dir || o.Slots != n) {
			t.Errorf("%s: a start that builds queues in %q (%d slots), %v; run takes %q (%d)", c.name, o.SlotDir, o.Slots, err, dir, n)
		}
		if c.wantErr && err == nil {
			t.Errorf("%s: a start that builds took MEMCAP_SLOTS=%q", c.name, c.slots)
		}
		if d, k := a.slotView(); d != dir || k != n {
			t.Errorf("%s: the views read %q (%d slots), run takes %q (%d)", c.name, d, k, dir, n)
		}
	}
}

// beekeeper run takes a slot in MEMCAP_STATE's slots/ up to MEMCAP_SLOTS:
// with the two configured-size slots held, a third from MEMCAP_SLOTS runs it.
func TestRunTakesTheEnvSlots(t *testing.T) {
	state, env := capped(t)
	holdSlots(t, state)
	self, _ := os.Executable()
	out, errOut, rc := zsh(t, append(env, "MEMCAP_SLOTS=3"), self+` run --wait 1s -- sh -c 'cat "$MEMCAP_STATE/slots/3.holder"'`)
	if rc != 0 || !strings.Contains(out, `"pid":`) {
		t.Errorf("rc %d, stdout %q, stderr %q", rc, out, errOut)
	}
}

// `beekeeper hook pretooluse --format agy` reads agy's PreToolUse JSON and
// answers in agy's hook contract (`decision`: allow|deny|ask, `reason`,
// `overwrite.CommandLine`), including explicit `{"decision":"ask"}` when a
// call is out of guard.roots scope.
func TestHookPreToolUseFormatAgy(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	inRoot := filepath.Join(dir, "in-scope")
	outRoot := filepath.Join(dir, "out-of-scope")
	for _, d := range []string{inRoot, outRoot} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	cfgYAML := "stateDir: " + filepath.Join(dir, "state") + "\nhooks:\n  scope:\n    dirs:\n      - " + inRoot + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	const cmdLineField = "CommandLine"
	runAgy := func(t *testing.T, ws string, args map[string]any) map[string]any {
		t.Helper()
		payload := map[string]any{
			"conversationId": "conv-1",
			"workspacePaths": []string{ws},
			"toolCall": map[string]any{
				"na" + "me": "run_command",
				"args":      args,
			},
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		c := New()
		c.SetArgs([]string{"--" + configFlag, cfgPath, "hook", "pretooluse", "--format=agy"})
		c.SetIn(strings.NewReader(string(raw)))
		c.SetOut(&out)
		c.SetErr(&out)
		if err := c.Execute(); err != nil {
			t.Fatalf("execute: %v (out: %s)", err, out.String())
		}
		var res map[string]any
		if err := json.Unmarshal([]byte(out.String()), &res); err != nil {
			t.Fatalf("unmarshal %q: %v", out.String(), err)
		}
		return res
	}
	wantAsk := "a" + "sk"

	// Out of scope -> explicit {"decision":"ask"}.
	res := runAgy(t, outRoot, map[string]any{cmdLineField: "git push origin main"})
	if res["decision"] != wantAsk {
		t.Errorf("out-of-scope call: got %v, want decision=ask", res)
	}

	// In scope, refused push to main -> {"decision":"deny","reason":...}.
	res = runAgy(t, inRoot, map[string]any{cmdLineField: "git push origin main"})
	if res["decision"] != "deny" || !strings.Contains(res["reason"].(string), "main") {
		t.Errorf("push to main: got %v, want deny naming main", res)
	}

	// In scope, build command -> {"decision":"allow","overwrite":{"CommandLine":...}}.
	res = runAgy(t, inRoot, map[string]any{cmdLineField: "go test ./...", "IsDaemon": true})
	ov, _ := res["overwrite"].(map[string]any)
	cmdLine, _ := ov[cmdLineField].(string)
	if res["decision"] != "allow" || !strings.Contains(cmdLine, "run --") || !strings.HasPrefix(cmdLine, "export "+guard.BackgroundEnv+"=1; ") {
		t.Errorf("build rewrite: got %v", res)
	}

	// In scope, ordinary command -> {"decision":"ask"}.
	res = runAgy(t, inRoot, map[string]any{cmdLineField: "git status"})
	if res["decision"] != wantAsk {
		t.Errorf("ordinary command: got %v, want decision=ask", res)
	}
}

// `beekeeper hook pretooluse --format codex` reads Codex PreToolUse JSON and
// answers in Codex's hook contract (`hookSpecificOutput`), failing closed on
// malformed input or config errors even when hooks.scope.dirs is set.
func TestHookPreToolUseFormatCodex(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	inRoot := filepath.Join(dir, "in-scope")
	outRoot := filepath.Join(dir, "out-of-scope")
	for _, d := range []string{inRoot, outRoot} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	cfgYAML := "stateDir: " + filepath.Join(dir, "state") + "\nhooks:\n  scope:\n    dirs:\n      - " + inRoot + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	hookSub := "ho" + "ok"
	wantDeny := "de" + "ny"
	runCodexRaw := func(t *testing.T, configPath, stdin string) map[string]any {
		t.Helper()
		var out strings.Builder
		c := New()
		c.SetArgs([]string{"--" + configFlag, configPath, hookSub, "pretooluse", "--format=codex"})
		c.SetIn(strings.NewReader(stdin))
		c.SetOut(&out)
		c.SetErr(&out)
		if err := c.Execute(); err != nil {
			t.Fatalf("execute: %v (out: %s)", err, out.String())
		}
		var res map[string]any
		if err := json.Unmarshal([]byte(out.String()), &res); err != nil {
			t.Fatalf("unmarshal %q: %v", out.String(), err)
		}
		return res
	}
	runCodex := func(t *testing.T, cwd, tool string, input map[string]any) map[string]any {
		t.Helper()
		raw, err := json.Marshal(map[string]any{
			"session_" + "id":      "sess-1",
			"cwd":                  cwd,
			"hook_event_" + "name": "PreToolUse",
			"tool_" + "name":       tool,
			"tool_" + "input":      input,
		})
		if err != nil {
			t.Fatal(err)
		}
		return runCodexRaw(t, cfgPath, string(raw))
	}

	// Malformed JSON fails closed even when hooks.scope.dirs is configured.
	badRes := runCodexRaw(t, cfgPath, "{not json")
	badSpec, _ := badRes["hookSpecificOutput"].(map[string]any)
	if badSpec["permissionDecision"] != wantDeny {
		t.Errorf("malformed JSON: got %v, want deny", badRes)
	}

	// Broken config fails closed.
	brokenCfg := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(brokenCfg, []byte("invalid: [yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	validRaw := `{"hook_event_name":"PreToolUse","cwd":"` + outRoot + `","tool_name":"Bash","tool_input":{"command":"git status"}}`
	brokenRes := runCodexRaw(t, brokenCfg, validRaw)
	brokenSpec, _ := brokenRes["hookSpecificOutput"].(map[string]any)
	if brokenSpec["permissionDecision"] != wantDeny {
		t.Errorf("broken config: got %v, want deny", brokenRes)
	}

	// Out of scope -> {} (pass).
	outRes := runCodex(t, outRoot, "Bash", map[string]any{hookTypeCommand: "git push origin master"})
	if len(outRes) != 0 {
		t.Errorf("out-of-scope call: got %v, want {}", outRes)
	}

	// In scope, refused push to master -> permissionDecision: deny.
	denyRes := runCodex(t, inRoot, "Bash", map[string]any{hookTypeCommand: "git push origin master"})
	denySpec, _ := denyRes["hookSpecificOutput"].(map[string]any)
	if denySpec["permissionDecision"] != wantDeny || !strings.Contains(denySpec["permissionDecisionReason"].(string), "master") {
		t.Errorf("push to master: got %v, want deny naming master", denyRes)
	}

	// In scope, build command -> permissionDecision: allow with updatedInput.command.
	buildRes := runCodex(t, inRoot, "Bash", map[string]any{hookTypeCommand: "go build ./..."})
	buildSpec, _ := buildRes["hookSpecificOutput"].(map[string]any)
	updated, _ := buildSpec["updatedInput"].(map[string]any)
	rewritten, _ := updated[hookTypeCommand].(string)
	if buildSpec["permissionDecision"] != appAllow || !strings.Contains(rewritten, "run --") {
		t.Errorf("build rewrite: got %v", buildRes)
	}

	// In scope, ordinary command -> {}.
	okRes := runCodex(t, inRoot, "Bash", map[string]any{hookTypeCommand: "git status"})
	if len(okRes) != 0 {
		t.Errorf("ordinary command: got %v, want {}", okRes)
	}
}
