package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
		"lanes: [{name: agent-platform, installation: gazelle, repositories: [giantswarm/agent-platform]}]\n" +
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
		st.Merges = []state.Merge{{Repo: "giantswarm/agent-platform", PR: 701, Lane: "agent-platform", Phase: state.Settling,
			Release: "v4.79.0", Roll: []string{"flux-giantswarm/agent-platform"}, Finished: now}}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	out := &syncBuffer{}
	w := (&app{cfg: c, store: store, now: now, out: out}).newWatcher(true, false)
	w.readHRs = readHRs
	w.spare.send = func(context.Context, string, string) error {
		t.Error("the test watch sent a message")
		return nil
	}
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
// within one interval, the polls go on, and the hung read is not started a
// second time while it runs.
func TestWatchSamplesTheMachineWhileALaneReadHangs(t *testing.T) {
	release := make(chan struct{})
	var reads atomic.Int32
	w, out := loopWatcher(t, "200ms", ", availMinMiB: 1000000000", func(context.Context, config.Lane) ([]merge.HelmRelease, error) {
		// A hung read ignores its context, like a kubectl whose credential
		// plugin still holds its pipes.
		reads.Add(1)
		<-release
		return nil, nil
	})
	defer close(release)
	start := time.Now()
	runWatch(t, w)
	if !eventually(200*time.Millisecond, func() bool { return strings.Contains(out.String(), "LOW RAM") }) {
		t.Fatalf("no LOW RAM line within one watch.interval while a lane read hangs:\n%s", out.String())
	}
	if !eventually(2*time.Second, func() bool { return w.polls.Load() >= 4 }) {
		t.Fatalf("%d polls in %s while a lane read hangs", w.polls.Load(), time.Since(start).Round(time.Millisecond))
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("the hung lane read was started %d times, want once", n)
	}
}

// While the CPU is under pressure the installation reads run slowReads
// times less often and at nice 10, said once as READS SLOWED.
func TestWatchSlowsInstallationReadsUnderCPUPressure(t *testing.T) {
	count := func(extra string) (reads, niced int32, out string) {
		var r, n atomic.Int32
		w, o := loopWatcher(t, "250ms", extra, func(ctx context.Context, _ config.Lane) ([]merge.HelmRelease, error) {
			r.Add(1)
			if proc.IsBackground(ctx) {
				n.Add(1)
			}
			return nil, nil
		})
		runWatch(t, w)
		time.Sleep(3 * time.Second)
		return r.Load(), n.Load(), o.String()
	}
	idle, idleNiced, _ := count(", loadMax: 1000000, cpuPSIMax: 1000")
	strained, strainedNiced, out := count(", loadMax: 1000000, cpuPSIMax: -1")
	if idle < 8 || strained*2 >= idle {
		t.Errorf("%d lane reads under CPU pressure against %d idle in 3s, want about a quarter", strained, idle)
	}
	if idleNiced != 0 || strained == 0 || strainedNiced < strained-1 {
		t.Errorf("%d of %d reads niced under CPU pressure, %d of %d idle", strainedNiced, strained, idleNiced, idle)
	}
	if strings.Count(out, "READS SLOWED: machine under CPU pressure") != 1 {
		t.Errorf("READS SLOWED is not said once:\n%s", out)
	}
}

// A loop whose run overruns its interval skips the ticks it missed.
func TestLoopSkipsMissedTicks(t *testing.T) {
	w := &watcher{}
	ctx, cancel := context.WithTimeout(context.Background(), 450*time.Millisecond)
	defer cancel()
	var runs []time.Time
	w.loop(ctx, 100*time.Millisecond, false, func(context.Context) {
		runs = append(runs, time.Now())
		if len(runs) == 1 {
			time.Sleep(250 * time.Millisecond)
		}
	})
	// Runs at 0 (overrunning to 250ms), 300 and 400: the ticks at 100 and
	// 200 are skipped, not run back to back at 250.
	if len(runs) != 3 {
		t.Fatalf("%d runs, want 3", len(runs))
	}
	if gap := runs[1].Sub(runs[0]); gap < 290*time.Millisecond {
		t.Fatalf("the run after an overrun started %s after it, a queued tick", gap)
	}
}
