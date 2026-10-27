package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The sessions of the stop's tests: a worker beekeeper started, a session
// the person started, and the guide; deskTimer is a desk's own unit.
const (
	deskTimer  = "desk-guard.timer"
	agentSeven = "Agent seven"
	untilReset = "until the reset"
	reportUnit = "beekeeper-report-9f9f9f9f.service"
	stopWorker = "5a1b2c3d-0000-4000-8000-000000000001" //nolint:gosec // a session id
	stopPerson = "6b2c3d4e-0000-4000-8000-000000000002"
	stopGuide  = "7c3d4e5f-0000-4000-8000-000000000003"
)

// haltLauncher is systemd --user with the units in active running and
// the installed units in installed; it records the stops and the starts.
type haltLauncher struct {
	platform.Launcher
	mu        sync.Mutex
	active    []string
	installed map[string]string
	stopped   []string
	resumed   []string
	// fail are the units whose stop or start fails.
	fail    map[string]bool
	started []platform.Unit
}

func (l *haltLauncher) Available() bool { return true }

func (l *haltLauncher) Running(_ context.Context, _ bool, patterns ...string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, u := range l.active {
		if slices.ContainsFunc(patterns, func(p string) bool { ok, _ := path.Match(p, u); return ok }) {
			out = append(out, u)
		}
	}
	return out
}

func (l *haltLauncher) State(_ context.Context, name string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.installed[name]
}

func (l *haltLauncher) Stop(_ context.Context, name string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail[name] {
		return errors.New("refused")
	}
	l.stopped = append(l.stopped, name)
	l.active = slices.DeleteFunc(l.active, func(u string) bool { return u == name })
	if _, ok := l.installed[name]; ok {
		l.installed[name] = "inactive"
	}
	return nil
}

func (l *haltLauncher) Resume(_ context.Context, name string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail[name] {
		return errors.New("refused")
	}
	l.resumed = append(l.resumed, name)
	l.installed[name] = unitActive
	return nil
}

func (l *haltLauncher) Start(u platform.Unit) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.started = append(l.started, u)
	return nil
}

func (l *haltLauncher) Failed(context.Context, ...string) []string { return nil }

// cliMachine runs one claude CLI per session id.
type cliMachine struct {
	platform.Machine
	ids []string
}

func (m cliMachine) Processes() (*proc.Table, error) {
	t := &proc.Table{ByPID: map[int]*proc.Process{}}
	for i, id := range m.ids {
		pid := 1000 + i
		t.ByPID[pid] = &proc.Process{PID: pid, PPID: 0, Comm: claudeComm, Args: []string{claudeComm, sessionIDFlag, id}}
	}
	return t, nil
}

// stopDesk is a desk of the stub app: the supervisor (agent one, s1), the
// guide, a worker beekeeper started with a wake timer due, and a session
// the person started, all of them running; the worker's turn, its pending
// reopen and the supervisor's wake run as units, the standby service and a
// desk timer are installed and run.
func stopDesk(t *testing.T) (*app, *haltLauncher, *[]string) {
	t.Helper()
	a, _ := stubApp(t)
	a.cfg.Stop.Units = []string{deskTimer}
	l := &haltLauncher{
		active: []string{"beekeeper-agent-5a1b2c3d.service", "beekeeper-reopen-5a1b2c3d-11111111.service",
			"beekeeper-wake-s1-22222222.service", reportUnit},
		installed: map[string]string{standbyUnit: unitActive, deskTimer: unitActive, "other.timer": unitActive},
	}
	plat.Launcher = l
	plat.Machine = cliMachine{Machine: plat.Machine, ids: []string{"s1", stopWorker, stopPerson, stopGuide}}
	var mu sync.Mutex
	ended := &[]string{}
	orig := endStopped
	endStopped = func(_ context.Context, s *claude.Session) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		*ended = append(*ended, s.ID)
		return 3, nil
	}
	t.Cleanup(func() { endStopped = orig })
	worker := state.Party{Session: stopWorker, Name: agentSeven}
	guide := state.Party{Session: stopGuide, Name: "Guide run 2"}
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide = &state.Role{Holder: &state.Supervisor{Party: guide, Since: a.now.Add(-time.Hour)}}
		st.Starts = append(st.Starts, state.Start{Party: worker, At: a.now.Add(-time.Hour)}, state.Start{Party: guide, At: a.now.Add(-time.Hour)},
			state.Start{Party: state.Party{Session: "s1", Name: agentOne}, At: a.now.Add(-time.Hour)})
		st.Agents = append(st.Agents, state.Agent{Party: worker, Task: rowlessTask})
		st.Timers = append(st.Timers, state.Timer{ID: 1, Due: a.now.Add(-time.Minute), Wake: worker.Name, What: "look again"})
		st.NextTimer = 2
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, l, ended
}

// A stop records itself, holds merges and GitHub, stops the turns, reopens
// and reports of the sessions it stops and the installed units that run,
// and ends the worker's session; it spares the supervisor's, the guide's
// and the person's sessions and their units. Resume lifts the holds and
// starts the installed units again, and names the session that waits.
func TestStopAndResume(t *testing.T) {
	a, l, ended := stopDesk(t)
	out := a.out.(interface{ String() string })
	if err := a.stop(context.Background(), "the person: stop all workers now"); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Stop == nil || st.Stop.Reason != "the person: stop all workers now" || st.Stop.By.Name != agentOne {
		t.Fatalf("stop record %+v", st.Stop)
	}
	for _, target := range stopHolds {
		if _, ok := activeHold(st, a, target); !ok {
			t.Errorf("%s not held:\n%s", target, out)
		}
	}
	slices.Sort(l.stopped)
	want := []string{"beekeeper-agent-5a1b2c3d.service", standbyUnit, "beekeeper-reopen-5a1b2c3d-11111111.service", reportUnit, deskTimer}
	slices.Sort(want)
	if !slices.Equal(l.stopped, want) {
		t.Errorf("stopped %v, want %v", l.stopped, want)
	}
	if !slices.Equal(*ended, []string{stopWorker}) {
		t.Errorf("ended %v, want only the worker's session", *ended)
	}
	if !slices.Equal(st.Stop.Units, []string{standbyUnit, deskTimer}) || !slices.Equal(st.Stop.Holds, stopHolds) || len(st.Stop.Sessions) != 1 {
		t.Errorf("stop record %+v", st.Stop)
	}
	if s := out.String(); !strings.Contains(s, "stop: held merges") || !strings.Contains(s, "stopped "+standbyUnit) || !strings.Contains(s, "until beekeeper resume") {
		t.Errorf("stop's output:\n%s", s)
	}

	// While it stands: no start, no wake, no reopen, no capacity, and the
	// wake timer waits.
	if _, err := a.startAgent(context.Background(), agentStart{name: "Agent eight", dir: t.TempDir()}); !refusedStop(err) {
		t.Errorf("start while stopped: %v", err)
	}
	if err := a.wakeAgent(context.Background(), state.Party{Name: agentOne}, agentSeven, "go on", ""); !refusedStop(err) {
		t.Errorf("wake while stopped: %v", err)
	}
	v, err := a.startCapacity(context.Background(), false)
	if err != nil || v.Room != 0 || !strings.Contains(v.verdict(), "no start: beekeeper is stopped") {
		t.Errorf("capacity while stopped: %v %+v", err, v)
	}
	if _, err := gateStart(v, true); !refusedStop(err) || strings.Contains(err.Error(), "--force") {
		t.Errorf("a forced start's gate while stopped: %v", err)
	}
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		_, _, fires, _ := settleTimers(st, nil, a.now)
		if len(fires) != 0 || len(st.Timers) != 1 {
			t.Errorf("a wake timer fired while stopped: %+v", fires)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.status()
	if err != nil || !strings.HasPrefix(s.line(), "STOPPED: beekeeper is stopped by "+agentOne) {
		t.Errorf("status while stopped: %v %q", err, s.line())
	}

	if err := a.resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, _ = a.store.Read()
	if st.Stop != nil || len(a.activeHolds(st)) != 0 {
		t.Errorf("after resume: stop %+v, holds %+v", st.Stop, st.Holds)
	}
	if !slices.Equal(l.resumed, []string{standbyUnit, deskTimer}) {
		t.Errorf("resumed %v", l.resumed)
	}
	if s := out.String(); !strings.Contains(s, "resume: lifted the hold on github") || !strings.Contains(s, "wait for a wake (beekeeper agents wake): ") {
		t.Errorf("resume's output:\n%s", s)
	}
	_, _, fires, _ := settleTimers(st, nil, a.now)
	if len(fires) != 1 {
		t.Errorf("the wake timer did not fire after the resume: %+v", fires)
	}
}

// A full hold that stood before the stop stays after the resume. One that
// lets a merge through, or that would end by itself during the stop, gives
// way to the stop's and is put back exactly; one whose time passed during
// the stop is not.
func TestResumeKeepsAndRestoresTheHoldsThatStoodBefore(t *testing.T) {
	window := state.Hold{Target: holdMerges, Reason: "a window", Except: refOne, ExceptSession: "a promoter", At: time.Now().UTC().Add(-time.Hour)}
	for name, tc := range map[string]struct {
		before   []state.Hold
		stopHeld []string
		after    []state.Hold
	}{
		"a full hold stays": {
			before:   []state.Hold{{Target: holdGitHub, Reason: "the budget is spent", At: time.Now().UTC()}},
			stopHeld: []string{holdMerges},
			after:    []state.Hold{{Target: holdGitHub, Reason: "the budget is spent"}},
		},
		"an exception is put back": {
			before:   []state.Hold{window},
			stopHeld: stopHolds,
			after:    []state.Hold{window},
		},
		"a timed hold is put back": {
			before:   []state.Hold{{Target: holdGitHub, Reason: untilReset, Until: time.Now().UTC().Add(time.Hour)}},
			stopHeld: stopHolds,
			after:    []state.Hold{{Target: holdGitHub, Reason: untilReset}},
		},
		"a timed hold that ended meanwhile is not": {
			before:   []state.Hold{{Target: holdGitHub, Reason: untilReset, Until: time.Now().UTC().Add(time.Second)}},
			stopHeld: stopHolds,
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, _, _ := stopDesk(t)
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) { st.Holds = tc.before; return nil, nil }); err != nil {
				t.Fatal(err)
			}
			if err := a.stop(context.Background(), "stop"); err != nil {
				t.Fatal(err)
			}
			st, _ := a.store.Read()
			if !slices.Equal(st.Stop.Holds, tc.stopHeld) {
				t.Fatalf("the stop's holds %v, want %v", st.Stop.Holds, tc.stopHeld)
			}
			for _, target := range stopHolds {
				if h, ok := activeHold(st, a, target); !ok || (target == holdMerges && h.Except != "") {
					t.Errorf("%s not held whole during the stop: %+v", target, h)
				}
			}
			a.now = time.Now().Add(time.Minute)
			if err := a.resume(context.Background()); err != nil {
				t.Fatal(err)
			}
			st, _ = a.store.Read()
			hs := a.activeHolds(st)
			if len(hs) != len(tc.after) {
				t.Fatalf("holds after resume: %+v, want %+v", hs, tc.after)
			}
			for i, want := range tc.after {
				got := hs[i]
				if got.Target != want.Target || got.Reason != want.Reason || (want.Except == "" && got.Except != "") {
					t.Errorf("hold %d after resume: %+v, want %+v", i, got, want)
				}
				if want.Except != "" && (got.Except != want.Except || got.ExceptSession != want.ExceptSession || !got.At.Equal(want.At)) {
					t.Errorf("the window came back changed: %+v, want %+v", got, want)
				}
			}
		})
	}
}

// A second stop keeps the first one's record and stops what came up since;
// a resume without a stop says so.
func TestStopAgainAndResumeUnstopped(t *testing.T) {
	a, l, _ := stopDesk(t)
	if err := a.resume(context.Background()); err != nil || !strings.Contains(a.out.(interface{ String() string }).String(), "beekeeper is not stopped") {
		t.Fatalf("resume unstopped: %v", err)
	}
	if err := a.stop(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	l.active = append(l.active, "beekeeper-wake-5a1b2c3d-33333333.service")
	l.mu.Unlock()
	if err := a.stop(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	if st.Stop.Reason != "first" || !slices.Contains(l.stopped, "beekeeper-wake-5a1b2c3d-33333333.service") {
		t.Errorf("second stop: record %+v, stopped %v", st.Stop, l.stopped)
	}
	if !slices.Equal(st.Stop.Units, []string{standbyUnit, deskTimer}) {
		t.Errorf("units %v", st.Stop.Units)
	}
}

// refusedStop reports whether err is the stop's refusal.
func refusedStop(err error) bool {
	var e *exitError
	return errors.As(err, &e) && e.code == ExitRefused && strings.Contains(e.msg, "beekeeper is stopped")
}

// While stopped, a pending reopen leaves its session closed and starts no
// unit, and the standby watch starts no successor for a gone supervisor,
// revives none and reopens nothing: nothing starts.
func TestNothingStartsWhileStopped(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), true)
	l := &haltLauncher{active: []string{"beekeeper-reopen-4579230b-11111111.service"}, installed: map[string]string{}}
	useLauncher(t, l)
	var calls atomic.Int32
	w.stand = standbyWatch{
		turning:   func(context.Context, string) bool { return false },
		reopening: func(context.Context, string) bool { return false },
		send:      func(context.Context, string, string) error { calls.Add(1); return nil },
		open:      func(context.Context, string, bool) error { calls.Add(1); return nil },
		revive:    func(context.Context, role, state.Party, string) error { calls.Add(1); return nil },
		succeed: func(context.Context, role, state.Party) (state.Party, error) {
			calls.Add(1)
			return state.Party{}, nil
		},
		importRow: func(context.Context, string, string) (string, error) { calls.Add(1); return "", nil },
	}
	t.Cleanup(w.stand.inflight.Wait)
	w.now = relayNow
	gone := state.Party{Session: "sS9", HostSession: "local_sS9", Name: supervisorRole.runName(9)}
	worker := state.Party{Session: runSixtySeven, HostSession: "local_" + runSixtySeven, Name: agentSeven}
	err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		// A supervisor gone past its restart grace, no start of beekeeper's:
		// its successor is due.
		st.Supervisor = &state.Supervisor{Party: gone, Since: relayNow.Add(-time.Hour)}
		st.SupervisorCLI = &state.CLI{Supervisor: gone, Since: relayNow.Add(-time.Hour), PID: 90, Gone: relayNow.Add(-time.Hour)}
		st.Starts = append(st.Starts, state.Start{Party: worker, At: relayNow.Add(-time.Hour)})
		st.Agents = append(st.Agents, state.Agent{Party: worker, Task: rowlessTask})
		st.Stop = &state.Stop{Reason: "the person said stop", By: state.Party{Name: "Agent one"}, At: relayNow.Add(-time.Minute)}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Duration{0, time.Minute, 10 * time.Minute} {
		w.now = relayNow.Add(at)
		w.pending(context.Background(), nil)
		w.stand.inflight.Wait()
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d starts, revives, sends or imports while stopped:\n%s", n, out)
	}
	if s := out.String(); !strings.Contains(s, "SUPERVISOR GONE") || !strings.Contains(s, stoppedNothing) || strings.Count(s, "STOPPED: beekeeper is stopped") != 1 {
		t.Errorf("watch lines:\n%s", s)
	}

	// The pending reopen's run, and its detach, leave the session closed.
	a := w.app
	var buf strings.Builder
	a.out = &buf
	if err := a.reopenSession(context.Background(), runSixtySeven, "beekeeper-agent-4579230b.service"); err != nil {
		t.Fatal(err)
	}
	if err := a.detachReopen(runSixtySeven); err != nil {
		t.Fatal(err)
	}
	if len(l.started) != 0 || strings.Count(buf.String(), "left closed: beekeeper is stopped") != 2 {
		t.Errorf("reopen while stopped: started %v:\n%s", l.started, buf.String())
	}

	// The resume lifts it: the watch says it ended.
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) { st.Stop = nil; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	a.out = out
	w.sayStop(&state.State{})
	if !strings.Contains(out.String(), "ENDED STOPPED") {
		t.Errorf("no ENDED line after the resume:\n%s", out)
	}
}

// A stop ends the devctl of a running gated merge before it merges, through
// its recorded merge-child, and says so.
func TestStopEndsARunningMergesDevctl(t *testing.T) {
	a, _, _ := stopDesk(t)
	child := os.Getpid() // alive; endChild is stubbed
	base := mergeBase(a.store.Dir(), scratchRepo, 7)
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".pid", []byte(strconv.Itoa(child)), 0o600); err != nil {
		t.Fatal(err)
	}
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = append(st.Merges, state.Merge{Repo: scratchRepo, PR: 7, Lane: scratchRepo, Phase: state.Running, Child: child},
			state.Merge{Repo: scratchRepo, PR: 8, Lane: scratchRepo, Phase: state.Waiting})
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var ended []int
	orig := endChild
	endChild = func(pid int) error { ended = append(ended, pid); return nil }
	t.Cleanup(func() { endChild = orig })
	if err := a.stop(context.Background(), "stop"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ended, []int{child}) {
		t.Errorf("ended %v, want the running merge's devctl %d", ended, child)
	}
	if s := a.out.(interface{ String() string }).String(); !strings.Contains(s, "ended the devctl of o/r#7") || !strings.Contains(s, "before it merged") {
		t.Errorf("stop's output:\n%s", s)
	}
}

// A unit that does not stop and a session that does not end make the stop
// fail, named, without its all-clear; the stop stands. A unit that does not
// start again keeps beekeeper stopped with that unit to start; the next
// resume, once it starts, lifts the stop.
func TestStopAndResumeFailures(t *testing.T) {
	a, l, _ := stopDesk(t)
	out := a.out.(interface{ String() string })
	l.fail = map[string]bool{reportUnit: true}
	endStopped = func(context.Context, *claude.Session) (int, error) {
		return 0, errors.New("the CLI still runs after SIGKILL")
	}
	err := a.stop(context.Background(), "stop")
	if err == nil || !strings.Contains(err.Error(), "2 of its steps failed") {
		t.Fatalf("stop with failures: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "FAILED stopping beekeeper-report-9f9f9f9f.service") || !strings.Contains(s, "FAILED ending") || strings.Contains(s, "until beekeeper resume") {
		t.Errorf("stop's output:\n%s", s)
	}
	st, _ := a.store.Read()
	if st.Stop == nil || len(a.activeHolds(st)) != 2 {
		t.Fatalf("the stop does not stand: %+v", st.Stop)
	}

	l.fail = map[string]bool{deskTimer: true}
	err = a.resume(context.Background())
	if err == nil || !strings.Contains(err.Error(), "beekeeper stays stopped") || !strings.Contains(out.String(), "FAILED starting "+deskTimer) {
		t.Fatalf("resume with a unit that does not start: %v\n%s", err, out)
	}
	st, _ = a.store.Read()
	if st.Stop == nil || !slices.Equal(st.Stop.Units, []string{deskTimer}) || len(a.activeHolds(st)) != 2 {
		t.Fatalf("after the failed resume: stop %+v, holds %+v", st.Stop, st.Holds)
	}
	l.fail = nil
	if err := a.resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, _ = a.store.Read()
	if st.Stop != nil || len(a.activeHolds(st)) != 0 || !slices.Equal(l.resumed, []string{standbyUnit, deskTimer}) {
		t.Errorf("after the resume: stop %+v, holds %+v, resumed %v", st.Stop, st.Holds, l.resumed)
	}
}

// While stopped, agents desktop, a standby revive and a hand-over are
// refused, the stop's reason named.
func TestStopRefusesDesktopReviveAndHandover(t *testing.T) {
	a, _, _ := stopDesk(t)
	if err := a.stop(context.Background(), "the person said stop"); err != nil {
		t.Fatal(err)
	}
	c := a.agentDesktopCmd()
	c.SetArgs([]string{agentSeven})
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	if err := c.Execute(); !refusedStop(err) {
		t.Errorf("agents desktop while stopped: %v", err)
	}
	if err := a.reviveFromWatch(context.Background(), supervisorRole, state.Party{Session: stopWorker, Name: agentSeven}, "resume"); !refusedStop(err) {
		t.Errorf("revive while stopped: %v", err)
	}
	st, _ := a.store.Read()
	i, err := findAgent(st, agentSeven)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	hook := `{"hooks": {"PermissionRequest": [{"matcher": "*", "hooks": [{"type": "command", "command": "beekeeper hook permissionrequest"}]}]}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(hook), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.handOver(context.Background(), handover{agent: st.Agents[i], dir: dir}); !refusedStop(err) || !strings.Contains(err.Error(), "the person said stop") {
		t.Errorf("hand-over while stopped: %v", err)
	}
}
