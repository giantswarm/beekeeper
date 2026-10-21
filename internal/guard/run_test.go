package guard

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/gocache"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/platform"
)

func TestCommandHead(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{testShell, "-c", "cd /x &&\n  go test ./...  "}, "cd /x && go test ./..."},
		{[]string{"/usr/bin/bash", "-c", "make test", "arg0"}, "make test"},
		{[]string{"python3", "-c", "bytearray(200<<20)"}, "python3 -c bytearray(200<<20)"},
		{[]string{"go", "test", strings.Repeat("x", 200)}, "go test " + strings.Repeat("x", 91) + "…"},
		// too long: the heavy command leads, not the checkout before it
		{[]string{testShell, "-c", slotIncident}, "… go test -race -timeout 25m -run '^TestSlackProof$' -count=150 -v ./internal/lab/ 2>&1 | /usr/bin/…"},
		{[]string{testShell, "-c", "git -C " + strings.Repeat("x", 100) + " switch -q main\ngo build ./..."}, "… go build ./..."},
		// too long with no heavy command: the start, clipped
		{[]string{testShell, "-c", "git -C " + strings.Repeat("x", 100) + " checkout -q -- ."}, "git -C " + strings.Repeat("x", 92) + "…"},
	} {
		if got := CommandHead(c.argv); got != c.want {
			t.Errorf("CommandHead(%q) = %q, want %q", c.argv, got, c.want)
		}
	}
}

// slotIncident is a compound command whose start (an assignment, a git
// checkout, a worktree removal) fills a slot record's width; the go test
// after it is what holds the slot.
const slotIncident = `S=/tmp/claude-1000/-home-teemow-projects-klaus-lab/5df9a204-e414-4e77-b24b-b146480a8572/scratchpad/amp; ` +
	`git -C "$S" checkout -q -- . && git -C ~/projects/giantswarm/agentlab worktree remove "$S"; ` +
	`cd ~/.local/state/klaus-lab/worktrees/agentlab/fix-slackproof-flake && go test -race -timeout 25m -run '^TestSlackProof$' -count=150 -v ./internal/lab/ 2>&1 | /usr/bin/grep -E "^panic|^(--- |ok|FAIL)" | sort | uniq -c`

func TestHolderNamesTheHeavyCommand(t *testing.T) {
	o := Options{SlotDir: t.TempDir(), Max: "12G"}
	writeHolder(o, 1, []string{testShell, "-c", slotIncident})
	b, err := os.ReadFile(filepath.Join(o.SlotDir, "1.holder"))
	if err != nil {
		t.Fatal(err)
	}
	var h holderRecord
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h.Cmd, "… go test -race -timeout 25m -run '^TestSlackProof$' -count=150") || strings.Contains(h.Cmd, "checkout") ||
		strings.Contains(h.Cmd, `"`) || len([]rune(h.Cmd)) > 140 {
		t.Errorf("holder cmd %q: want the go test first, no checkout, no double quote, at most 140 characters", h.Cmd)
	}
}

func TestRunEventDetail(t *testing.T) {
	d := "memcap-2516344-492870.scope slot 1 max 12G: go test ./... -run 'A: B'"
	if RunScope(d) != "memcap-2516344-492870.scope" || RunCommand(d) != "go test ./... -run 'A: B'" {
		t.Errorf("scope %q, command %q", RunScope(d), RunCommand(d))
	}
}

// trueCmd is the command the slot tests run.
var trueCmd = []string{"true"}

// fakeCapper runs the command uncapped and records the slots it was asked
// to cap and to run in.
type fakeCapper struct {
	capped, ran []string
	// down makes it unavailable: no user systemd, no broker.
	down bool
}

func (f *fakeCapper) Available() bool                     { return !f.down }
func (*fakeCapper) Capped() bool                          { return false }
func (*fakeCapper) Adopt(int, string, platform.Cap) error { return nil }
func (*fakeCapper) Reap(string) ([]int, error)            { return nil, nil }
func (f *fakeCapper) CapSlot(c platform.Cap) error {
	f.capped = append(f.capped, c.Slice)
	return nil
}
func (f *fakeCapper) Command(_ string, c platform.Cap, argv []string) (*exec.Cmd, error) {
	f.ran = append(f.ran, c.Slice)
	return exec.Command(argv[0], argv[1:]...), nil //nolint:gosec // the test's own command
}

// holdSlot holds slot n of dir for session as a running beekeeper run does.
func holdSlot(t *testing.T, dir string, n int, session string) {
	t.Helper()
	l := flock.New(filepath.Join(dir, strconv.Itoa(n)+".lock"))
	if ok, err := l.TryLock(); err != nil || !ok {
		t.Fatalf("lock slot %d: %v", n, err)
	}
	t.Cleanup(func() { _ = l.Unlock() })
	rec := fmt.Sprintf(`{"pid":1,"session":%q,"cwd":"/","cmd":"agentlab backstage-test","max":"12G","since":"2026-10-04T14:00:00Z"}`, session)
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(n)+".holder"), []byte(rec+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunSharesItsSessionsSlot(t *testing.T) {
	f := &fakeCapper{}
	saved := plat.Capper
	plat.Capper = f
	t.Cleanup(func() { plat.Capper = saved })
	dir := t.TempDir()
	o := Options{Max: "1M", Swap: "0", SlotDir: dir, Slots: 2, Stderr: io.Discard}
	holdSlot(t, dir, 2, "parent")
	holdSlot(t, dir, 1, "other")

	t.Setenv("CLAUDE_CODE_SESSION_ID", "parent")
	start := time.Now()
	if rc := Run(o, trueCmd); rc != 0 {
		t.Fatalf("a run inside its session's slot: exit %d, want 0", rc)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("a run inside its session's slot waited %s", time.Since(start))
	}
	if len(f.capped) != 0 || !slices.Equal(f.ran, []string{o.SlotSlice(2)}) {
		t.Errorf("capped slots %v, ran in %v: want none capped, ran in the shared slot 2", f.capped, f.ran)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "2.holder")) //nolint:gosec // the test's own temp dir
	if !strings.Contains(string(raw), `"session":"parent"`) {
		t.Errorf("the shared slot's holder record changed: %s", raw)
	}

	t.Setenv("CLAUDE_CODE_SESSION_ID", "outside")
	if rc := Run(o, trueCmd); rc != ExitBusy {
		t.Errorf("a run outside a slot with every slot held: exit %d, want %d", rc, ExitBusy)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	if rc := Run(o, trueCmd); rc != ExitBusy {
		t.Errorf("a run of no session with every slot held: exit %d, want %d", rc, ExitBusy)
	}
}

func TestRunTakesAndCapsAFreeSlot(t *testing.T) {
	f := &fakeCapper{}
	saved := plat.Capper
	plat.Capper = f
	t.Cleanup(func() { plat.Capper = saved })
	dir := t.TempDir()
	holdSlot(t, dir, 1, "other")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "fresh")
	o := Options{Max: "1M", Swap: "0", SlotDir: dir, Slots: 2, Stderr: io.Discard}
	if rc := Run(o, trueCmd); rc != 0 {
		t.Fatalf("exit %d, want 0", rc)
	}
	if !slices.Equal(f.capped, []string{o.SlotSlice(2)}) || !slices.Equal(f.ran, []string{o.SlotSlice(2)}) {
		t.Errorf("capped slots %v, ran in %v: want slot 2 capped and run in", f.capped, f.ran)
	}
	if _, err := os.Stat(filepath.Join(dir, "2.holder")); !os.IsNotExist(err) {
		t.Errorf("slot 2's holder record outlived the run: %v", err)
	}
}

func TestRunJoinsTheSlotItsSessionTakesDuringTheWait(t *testing.T) {
	f := &fakeCapper{}
	saved := plat.Capper
	plat.Capper = f
	t.Cleanup(func() { plat.Capper = saved })
	dir := t.TempDir()
	l := flock.New(filepath.Join(dir, "1.lock"))
	if ok, err := l.TryLock(); err != nil || !ok {
		t.Fatalf("lock: %v", err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "parent")
	go func() {
		time.Sleep(time.Second)
		_ = os.WriteFile(filepath.Join(dir, "1.holder"), []byte(`{"pid":1,"session":"parent"}`+"\n"), 0o600)
	}()
	t.Cleanup(func() { _ = l.Unlock() })
	o := Options{Max: "1M", Swap: "0", Wait: 20 * time.Second, SlotDir: dir, Slots: 1, Stderr: io.Discard}
	if rc := Run(o, trueCmd); rc != 0 {
		t.Fatalf("exit %d, want 0", rc)
	}
	if len(f.capped) != 0 || !slices.Equal(f.ran, []string{o.SlotSlice(1)}) {
		t.Errorf("capped slots %v, ran in %v: want none capped, ran in the shared slot 1", f.capped, f.ran)
	}
}

// busySlices is the machine with slot slices that still run scopes.
type busySlices struct {
	platform.Machine
	slices map[string]*machine.SlotSlice
}

func (b busySlices) SlotSlice(slice string) *machine.SlotSlice { return b.slices[slice] }

// fakeSlices puts the slot slices' runs into the machine the test's runs read.
func fakeSlices(t *testing.T, s map[string]*machine.SlotSlice) {
	t.Helper()
	saved := plat.Machine
	plat.Machine = busySlices{Machine: saved, slices: s}
	t.Cleanup(func() { plat.Machine = saved })
}

// A slot whose holder has ended while a run that joined it still runs in
// its slice is not free: capping the slice at a smaller --max would kill at
// the slice's cap (systemd-run at 0 MiB, before the command starts).
func TestRunLeavesASlotWhoseSliceStillRuns(t *testing.T) {
	f := &fakeCapper{}
	saved := plat.Capper
	plat.Capper = f
	t.Cleanup(func() { plat.Capper = saved })
	dir := t.TempDir()
	o := Options{Max: "1M", Swap: "0", SlotDir: dir, Slots: 2, Stderr: io.Discard}
	fakeSlices(t, map[string]*machine.SlotSlice{
		o.SlotSlice(1): {Scopes: []string{"memcap-4711-000001.scope"}, CurrentMiB: 6778},
	})
	t.Setenv("CLAUDE_CODE_SESSION_ID", "fresh")
	if rc := Run(o, trueCmd); rc != 0 {
		t.Fatalf("exit %d, want 0", rc)
	}
	if !slices.Equal(f.capped, []string{o.SlotSlice(2)}) || !slices.Equal(f.ran, []string{o.SlotSlice(2)}) {
		t.Errorf("capped slots %v, ran in %v: want slot 1 left alone, slot 2 capped and run in", f.capped, f.ran)
	}

	f.capped, f.ran = nil, nil
	var stderr strings.Builder
	o.Slots, o.Wait, o.Stderr = 1, time.Second, &stderr
	if rc := Run(o, trueCmd); rc != ExitBusy {
		t.Errorf("the only slot's slice still runs: exit %d, want %d", rc, ExitBusy)
	}
	if len(f.capped) != 0 || len(f.ran) != 0 {
		t.Errorf("capped slots %v, ran in %v: want none", f.capped, f.ran)
	}
	if want := "slot 1: free, but its slice still runs memcap-4711-000001.scope (6778 MiB)"; !strings.Contains(stderr.String(), want) {
		t.Errorf("the wait does not name the slice's run %q:\n%s", want, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "1.holder")); !os.IsNotExist(err) {
		t.Errorf("the occupied slot kept a holder record: %v", err)
	}
}

// scopeCapper runs the command in a process group of its own, its scope,
// and reaps what the command leaves in the group, the left processes'
// ids read from the file the command writes them to.
type scopeCapper struct {
	fakeCapper
	cmd *exec.Cmd
	// pids is the file the command writes the ids it leaves behind to.
	pids string
	// lock is the run's slot lock, held: Reap finds it still held.
	lock   string
	reaped []string
	held   bool
}

func (s *scopeCapper) Command(name string, c platform.Cap, argv []string) (*exec.Cmd, error) {
	cmd, _ := s.fakeCapper.Command(name, c, argv)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	s.cmd = cmd
	return cmd, nil
}

func (s *scopeCapper) Reap(name string) ([]int, error) {
	s.reaped = append(s.reaped, name)
	if err := syscall.Kill(-s.cmd.Process.Pid, 0); err != nil {
		return nil, nil // the scope ended with its command
	}
	ok, err := flock.New(s.lock).TryLock()
	s.held = err == nil && !ok
	raw, _ := os.ReadFile(s.pids)
	var left []int
	for _, f := range strings.Fields(string(raw)) {
		if pid, err := strconv.Atoi(f); err == nil {
			left = append(left, pid)
		}
	}
	return left, syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
}

// A command that exits and leaves a background child in its scope (go
// test's envtest kube-apiserver) ends with the scope stopped before the
// slot is released: the slot is free for the next run at once.
func TestRunStopsWhatTheCommandLeavesInItsScope(t *testing.T) {
	dir := t.TempDir()
	f := &scopeCapper{pids: filepath.Join(t.TempDir(), "left"), lock: filepath.Join(dir, "1.lock")}
	saved := plat.Capper
	plat.Capper = f
	t.Cleanup(func() { plat.Capper = saved })
	t.Setenv("CLAUDE_CODE_SESSION_ID", "fresh")
	var stderr strings.Builder
	var ends []string
	o := Options{Max: "1M", Swap: "0", SlotDir: dir, Slots: 1, Wait: time.Second, Stderr: &stderr,
		Record: func(verb, detail string) {
			if verb == VerbEnd {
				ends = append(ends, detail)
			}
		}}
	leaves := []string{"sh", "-c", `sleep 300 & echo $! > "$0"`, f.pids}
	if rc := Run(o, leaves); rc != 0 {
		t.Fatalf("exit %d, want the command's 0:\n%s", rc, stderr.String())
	}
	raw, err := os.ReadFile(f.pids)
	if err != nil {
		t.Fatal(err)
	}
	child, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if len(f.reaped) != 1 || !f.held {
		t.Fatalf("reaped %v, slot held while reaping %v: want the run's scope reaped once, before the slot is released", f.reaped, f.held)
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(child, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(child, 0) == nil {
		_ = syscall.Kill(child, syscall.SIGKILL)
		t.Errorf("the left child %d runs on", child)
	}
	want := fmt.Sprintf("the command exited and left %d in %s.scope: stopped them, they hold slot 1 no longer", child, f.reaped[0])
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("run said:\n%s\nwant %q", stderr.String(), want)
	}
	if len(ends) != 1 || !strings.Contains(ends[0], fmt.Sprintf(", stopped what it left: %d: ", child)) {
		t.Errorf("end events %q: want the stop in the run's end", ends)
	}
	if s := machine.ReadSlots(dir, 1); !s[0].Free {
		t.Errorf("slot 1 after the run: %+v, want free", s[0])
	}
	// The next run takes the slot at once, and a command that leaves
	// nothing behind has nothing reaped and says nothing about it.
	stderr.Reset()
	if rc := Run(o, trueCmd); rc != 0 || stderr.Len() != 0 {
		t.Errorf("the next run: exit %d, said %q; want 0 and nothing", rc, stderr.String())
	}
}

func TestKillReport(t *testing.T) {
	const ownAdvice, sharedCap = "bound the parallelism", "shared"
	o := Options{Max: "6G", SlotDir: "/s"}
	unit := "memcap-1-000001"
	scope := "/user.slice/user-1000.slice/user@1000.service/memcap.slice/" + o.SlotSlice(1) + "/" + unit + ".scope"
	slice := path.Dir(scope)
	for _, c := range []struct {
		name   string
		shared bool
		kills  []machine.OOMKill
		want   []string
		not    string
	}{
		{"own scope's cap", false, []machine.OOMKill{{PID: 7, Task: "go", AnonMiB: 6100, Memcg: scope}},
			[]string{"the 6G cap killed: 7 (go) 6100 MiB", ownAdvice}, sharedCap},
		{"the held slot's slice, capped at the run's cap", false, []machine.OOMKill{{PID: 7, Task: "go", AnonMiB: 6100, Memcg: slice, TaskMemcg: scope}},
			[]string{"the 6G cap killed: 7 (go) 6100 MiB", ownAdvice}, sharedCap},
		{"systemd-run at the slice's cap", false, []machine.OOMKill{{PID: 8, Task: "systemd-run", Memcg: slice, TaskMemcg: scope}},
			[]string{"slot 1's cap (" + o.SlotSlice(1) + ") killed systemd-run before the command started: 8 (systemd-run) 0 MiB", "not this command"}, ownAdvice},
		{"systemd-run in a joined slot", true, []machine.OOMKill{{PID: 9, Task: "systemd-run", Memcg: slice, TaskMemcg: scope}},
			[]string{"before the command started"}, ownAdvice},
		{"the command at a joined slot's cap", true, []machine.OOMKill{{PID: 10, Task: "node", AnonMiB: 900, Memcg: slice, TaskMemcg: scope}},
			[]string{"slot 1's shared cap", "together filled it"}, ownAdvice},
		{"the command at its own cap in a joined slot", true, []machine.OOMKill{{PID: 11, Task: "node", AnonMiB: 6100, Memcg: scope}},
			[]string{"the 6G cap killed", ownAdvice}, sharedCap},
	} {
		what, advice, _ := killReport(o, unit, 1, c.shared, c.kills)
		line := what + " — " + advice
		for _, w := range c.want {
			if !strings.Contains(line, w) {
				t.Errorf("%s: %q lacks %q", c.name, line, w)
			}
		}
		if strings.Contains(line, c.not) {
			t.Errorf("%s: %q says %q", c.name, line, c.not)
		}
	}
}

func TestSlotSliceIsTheSlotDirectorysOwn(t *testing.T) {
	machine, scratch := Options{SlotDir: "/home/u/.local/state/memcap/slots"}, Options{SlotDir: "/tmp/test/slots"}
	a, b := machine.SlotSlice(1), scratch.SlotSlice(1)
	if a == b || strings.Count(a, "-") != 1 || !strings.HasPrefix(a, "memcap-slot1_") || !strings.HasSuffix(a, ".slice") {
		t.Errorf("SlotSlice: %q and %q", a, b)
	}
	if (Options{SlotDir: "/tmp/test/slots/"}).SlotSlice(1) != b || scratch.SlotSlice(2) == b {
		t.Error("SlotSlice: want one slice per clean directory and slot")
	}
	if got := (Options{SlotDir: t.TempDir(), Test: true}).SlotSlice(1); got != "memcap-slot1_test.slice" {
		t.Errorf("a test's SlotSlice = %q", got)
	}
}

func TestRunInTheSandboxGoesThroughTheBroker(t *testing.T) {
	host := &fakeCapper{down: true}
	saved := plat.Capper
	plat.Capper = host
	t.Cleanup(func() { plat.Capper = saved })
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sandboxed")
	broker := &fakeCapper{}
	o := Options{Max: "1M", Swap: "0", SlotDir: t.TempDir(), Slots: 1, Stderr: io.Discard, Sandbox: broker}
	if rc := Run(o, trueCmd); rc != 0 {
		t.Fatalf("a sandboxed run with a broker: exit %d, want 0", rc)
	}
	if !slices.Equal(broker.capped, []string{o.SlotSlice(1)}) || !slices.Equal(broker.ran, []string{o.SlotSlice(1)}) || len(host.ran) != 0 {
		t.Errorf("broker capped %v, ran %v; host ran %v", broker.capped, broker.ran, host.ran)
	}

	var stderr strings.Builder
	o.Sandbox, o.Stderr = &fakeCapper{down: true}, &stderr
	if rc := Run(o, []string{"false-is-never-run"}); rc != 1 || !strings.Contains(stderr.String(), "refused: the agent sandbox") {
		t.Errorf("a sandboxed run without a broker: exit %d, %q; want 1 and the refusal", rc, stderr.String())
	}
}

func TestResolveWait(t *testing.T) {
	for _, c := range []struct {
		bg, env, flag string
		flagSet       bool
		wait, from    string
		background    bool
	}{
		{wait: ForegroundWait, from: WaitFromForeground},
		{bg: "1", wait: BackgroundWait, from: WaitFromBackground, background: true},
		{bg: "1", env: "5m", wait: "5m", from: WaitFromEnv, background: true},
		{env: "5m", wait: "5m", from: WaitFromEnv},
		{bg: "1", env: "5m", flag: "2m", flagSet: true, wait: "2m", from: WaitFromFlag, background: true},
	} {
		t.Setenv(BackgroundEnv, c.bg)
		t.Setenv("MEMCAP_WAIT", c.env)
		wait, from, bg := ResolveWait(c.flag, c.flagSet)
		if wait != c.wait || from != c.from || bg != c.background {
			t.Errorf("%+v: got %s, %s, %v", c, wait, from, bg)
		}
	}
}

func TestWaitText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		2 * time.Hour: "120m", 60 * time.Minute: "60m", 8 * time.Minute: "8m", 10 * time.Minute: "10m", time.Second: "1s", 90 * time.Second: "1m30s",
	} {
		if got := (Options{Wait: d}).waitText(); got != want {
			t.Errorf("%s: %q, want %q", d, got, want)
		}
	}
	if got := (Options{Wait: time.Hour, WaitFrom: WaitFromBackground}).waitText(); got != "60m (the background default)" {
		t.Errorf("with its source: %q", got)
	}
}

// A run holds the Go build cache's use lock for its command: no trim starts
// while it runs, and a run waits for a trim under way, up to its wait.
func TestRunHoldsTheGoCacheAgainstATrim(t *testing.T) {
	f := &fakeCapper{}
	saved := plat.Capper
	plat.Capper = f
	t.Cleanup(func() { plat.Capper = saved })
	t.Setenv("CLAUDE_CODE_SESSION_ID", "fresh")
	cache := filepath.Join(t.TempDir(), "go-build")
	marker := filepath.Join(t.TempDir(), "compiling")
	o := Options{Max: "1M", Swap: "0", SlotDir: t.TempDir(), Slots: 1, Stderr: io.Discard, Wait: time.Second, GoCache: cache}

	rc := make(chan int, 1)
	go func() { rc <- Run(o, []string{"sh", "-c", "touch " + marker + "; sleep 2"}) }()
	for deadline := time.Now().Add(5 * time.Second); !exists(marker); time.Sleep(20 * time.Millisecond) {
		select {
		case c := <-rc:
			t.Fatalf("the run ended, exit %d, before its command started", c)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the run's command never started")
		}
	}
	if _, ok, err := gocache.TryTrim(cache); ok || err != nil {
		t.Fatalf("a trim began while a run compiled: %v, %v", ok, err)
	}
	if c := <-rc; c != 0 {
		t.Fatalf("exit %d, want 0", c)
	}
	release, ok, err := gocache.TryTrim(cache)
	if !ok || err != nil {
		t.Fatalf("the run kept the cache after it ended: %v, %v", ok, err)
	}
	defer release()
	_ = os.Remove(marker)
	if c := Run(o, []string{"sh", "-c", "touch " + marker}); c != ExitBusy || exists(marker) {
		t.Errorf("a run under a trim: exit %d, ran %v; want %d, not run", c, exists(marker), ExitBusy)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }
