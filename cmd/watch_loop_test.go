package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The lane and repository of the merge a loop test's watch settles.
const loopLane, loopRepo = "loop-lane", "giantswarm/loop-repo"

// syncBuffer is a watch's output read while the watch writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// loopWatcher is a standby watch polling every interval, with a merge
// settling on a lane whose HelmReleases readHRs reads: extra is appended to
// the watch configuration.
func loopWatcher(t *testing.T, interval, extra string, readHRs func(context.Context, config.Lane) ([]merge.HelmRelease, error)) (*watcher, *syncBuffer) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := "stateDir: " + dir + "\n" +
		"claude: {projectsDir: " + filepath.Join(dir, "projects") + ", sessionsDir: " + filepath.Join(dir, "sessions") + "}\n" +
		"lanes: [{name: " + loopLane + ", installation: gazelle, repositories: [" + loopRepo + "]}]\n" +
		"watch: {interval: " + interval + ", budgetEvery: 24h" + extra + "}\n"
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(c.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	err = store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = []state.Merge{{Repo: loopRepo, PR: 701, Lane: loopLane, Phase: state.Settling,
			Release: "v4.79.0", Roll: []string{"flux-giantswarm/agent-platform"}, Finished: now}}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	out := &syncBuffer{}
	w := (&app{cfg: c, store: store, now: now, out: out}).newWatcher(true, false)
	w.readHRs = readHRs
	w.stand.send = func(context.Context, string, string) error {
		t.Error("the test watch sent a message")
		return nil
	}
	w.stand.succeed = func(context.Context, role, state.Party) (state.Party, error) {
		t.Error("the test watch started a successor")
		return state.Party{}, errors.New("no successor in a test")
	}
	w.stand.turning = nil
	// The budget probe is due at once and would ask GitHub.
	w.lastBudget = now
	return w, out
}

// runWatch runs the watch until the test ends.
func runWatch(t *testing.T, w *watcher) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.run(ctx, false)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// fakeClock is a watch's time that moves only when its test steps it: no
// loop test waits on the wall clock, which a loaded machine stretches.
// Every wait on it ends in a failure after a minute, the one bound.
type fakeClock struct {
	t     *testing.T
	guard <-chan time.Time
	mu    sync.Mutex
	now   time.Time
	// timers are the armed timers; armed is signalled when one is added.
	timers map[*fakeTimer]bool
	armed  chan struct{}
}

type fakeTimer struct {
	at   time.Time
	fire chan time.Time
}

func newFakeClock(t *testing.T) *fakeClock {
	return &fakeClock{t: t, guard: time.After(time.Minute), now: time.Now(),
		timers: map[*fakeTimer]bool{}, armed: make(chan struct{}, 1)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tm := &fakeTimer{at: c.now.Add(d), fire: make(chan time.Time, 1)}
	if d <= 0 {
		tm.fire <- c.now
		return tm.fire, func() {}
	}
	c.timers[tm] = true
	select {
	case c.armed <- struct{}{}:
	default:
	}
	return tm.fire, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.timers, tm)
	}
}

// advance moves the time by d and fires the timers due.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.moveTo(c.now.Add(d))
}

func (c *fakeClock) moveTo(at time.Time) {
	c.now = at
	for tm := range c.timers {
		if !tm.at.After(at) {
			tm.fire <- at
			delete(c.timers, tm)
		}
	}
}

// settle waits until n timers are armed: every loop of the watch waits.
func (c *fakeClock) settle(n int) {
	c.t.Helper()
	for {
		c.mu.Lock()
		k := len(c.timers)
		c.mu.Unlock()
		if k >= n {
			return
		}
		select {
		case <-c.armed:
		case <-c.guard:
			c.t.Fatalf("%d of %d timers armed after a minute", k, n)
		}
	}
}

// step waits until n timers are armed and moves the time to the earliest.
func (c *fakeClock) step(n int) {
	c.t.Helper()
	c.settle(n)
	c.mu.Lock()
	defer c.mu.Unlock()
	var next time.Time
	for tm := range c.timers {
		if next.IsZero() || tm.at.Before(next) {
			next = tm.at
		}
	}
	c.moveTo(next)
}

// loopTimers are the timers a standby watch's loops arm when they all wait:
// the machine sample's and the poll's.
const loopTimers = 2

// eventually waits up to within for cond.
func eventually(within time.Duration, cond func() bool) bool {
	for end := time.Now().Add(within); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// A lane read that hangs, as kubectl does on a starved machine, delays
// neither the machine's lines nor the next poll: the memory breach is said
// before the first interval ends, the polls go on, and the hung read is not
// started a second time while it runs.
func TestWatchSamplesTheMachineWhileALaneReadHangs(t *testing.T) {
	needsPlatform(t)
	release := make(chan struct{})
	// Registered first, so it runs last: the hung read returns only after
	// the watch has stopped and its state directory is gone, and writes
	// nothing into the directory while it is being removed.
	t.Cleanup(func() { close(release) })
	var reads atomic.Int32
	w, out := loopWatcher(t, "200ms", ", availMinMiB: 1000000000", func(context.Context, config.Lane) ([]merge.HelmRelease, error) {
		// A hung read ignores its context, like a kubectl whose credential
		// plugin still holds its pipes.
		reads.Add(1)
		<-release
		return nil, nil
	})
	clk := newFakeClock(t)
	w.clock = clk
	runWatch(t, w)
	clk.settle(loopTimers)
	if !strings.Contains(out.String(), "LOW RAM") {
		t.Fatalf("no LOW RAM line within one watch.interval while a lane read hangs:\n%s", out.String())
	}
	start := clk.Now()
	for w.polls.Load() < 4 {
		clk.step(loopTimers)
		clk.settle(loopTimers)
	}
	// The hung read holds its poll up for at most one interval of the wall
	// clock, inside the poll's own: the polls at 0 to 3 intervals.
	if took := clk.Now().Sub(start); took != 3*200*time.Millisecond {
		t.Fatalf("%d polls in %s while a lane read hangs", w.polls.Load(), took)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("the hung lane read was started %d times, want once", n)
	}
}

// While the CPU is under pressure the installation reads run slowReads
// times less often and at nice 10, said once as READS SLOWED.
func TestWatchSlowsInstallationReadsUnderCPUPressure(t *testing.T) {
	needsPlatform(t)
	const intervals = 12
	count := func(extra string) (reads, niced int32, out string) {
		var r, n atomic.Int32
		w, o := loopWatcher(t, "250ms", extra, func(ctx context.Context, _ config.Lane) ([]merge.HelmRelease, error) {
			r.Add(1)
			if proc.IsBackground(ctx) {
				n.Add(1)
			}
			return nil, nil
		})
		clk := newFakeClock(t)
		w.clock = clk
		runWatch(t, w)
		for end := clk.Now().Add(intervals * 250 * time.Millisecond); clk.Now().Before(end); {
			clk.step(loopTimers)
		}
		// The last poll is done once its loop waits again.
		clk.settle(loopTimers)
		return r.Load(), n.Load(), o.String()
	}
	idle, idleNiced, _ := count(", loadMax: 1000000, cpuPSIMax: 1000")
	strained, strainedNiced, out := count(", loadMax: 1000000, cpuPSIMax: -1")
	// A read every poll, at 0 to 12 intervals, against one every slowReads.
	if idle != intervals+1 || strained != intervals/slowReads+1 {
		t.Errorf("%d lane reads under CPU pressure against %d idle in %d intervals, want %d and %d",
			strained, idle, intervals, intervals/slowReads+1, intervals+1)
	}
	if idleNiced != 0 || strainedNiced != strained {
		t.Errorf("%d of %d reads niced under CPU pressure, %d of %d idle", strainedNiced, strained, idleNiced, idle)
	}
	if strings.Count(out, "READS SLOWED: machine under CPU pressure") != 1 {
		t.Errorf("READS SLOWED is not said once:\n%s", out)
	}
}

// A loop whose run overruns its interval skips the ticks it missed.
func TestLoopSkipsMissedTicks(t *testing.T) {
	clk := newFakeClock(t)
	w := &watcher{clock: clk}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := clk.Now()
	var runs []time.Duration
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.loop(ctx, 100*time.Millisecond, false, func(context.Context) {
			runs = append(runs, clk.Now().Sub(start))
			switch len(runs) {
			case 1:
				clk.advance(250 * time.Millisecond)
			case 3:
				cancel()
			}
		})
	}()
	clk.step(1)
	clk.step(1)
	<-done
	// Runs at 0 (overrunning to 250ms), 300 and 400: the ticks at 100 and
	// 200 are skipped, not run back to back at 250.
	want := []time.Duration{0, 300 * time.Millisecond, 400 * time.Millisecond}
	if !slices.Equal(runs, want) {
		t.Fatalf("runs at %v, want %v", runs, want)
	}
}
