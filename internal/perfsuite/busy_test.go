//go:build perf && linux

package perfsuite

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// A busy desk rewrites its state about once a second: every session, runner
// and command writes it, and most writes append an event. The processes that
// run as long as the desk (the loop and the two feeds) read what a rewrite
// changed, not the whole state again for each: beside such a writer each feed
// costs at most busyFeedCPU of one core, and the loop busyLoopCPU outside its
// children.
var (
	busyRewriteEvery = time.Second
	busyFeedCPU      = limit{Max: 0.05}
	busyLoopCPU      = limit{Max: 0.25}
)

// rewriteEvery rewrites d's state every interval until the returned stop runs,
// as a busy desk's writers do: one worker's activity and one event a rewrite.
// It returns how many rewrites it made so far.
func (d *desk) rewriteEvery(t *testing.T, interval time.Duration) (count func() int64, stop func()) {
	t.Helper()
	var n atomic.Int64
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			now := time.Now().UTC()
			err := d.store.Update(func(st *state.State) ([]state.Event, error) {
				if len(st.Agents) > 0 {
					st.Agents[i%len(st.Agents)].ActivityAt = now
				}
				return []state.Event{{At: now, By: state.Party{Name: fmt.Sprintf("perf worker %d", i%deskSessions), Person: fixturePerson}, Verb: "perf.rewrite", Detail: fmt.Sprintf("[perf] rewrite %d", i)}}, nil
			})
			if err != nil {
				t.Errorf("rewrite %d: %v", i, err)
				return
			}
			n.Add(1)
		}
	})
	var once sync.Once
	stop = func() { once.Do(func() { close(done); wg.Wait() }) }
	t.Cleanup(stop)
	return n.Load, stop
}

// busyUsage is a running process's cumulative cost: CPU of its own and with
// its reaped children, and bytes read.
type busyUsage struct {
	own, cpu time.Duration
	read     int64
}

// busyProcess is one of the processes a busy-desk window measures.
type busyProcess struct {
	name   string
	budget limit
	usage  func() busyUsage
	files  *fileReads
}

// busyWindow is what each process cost over one window, per second of it and
// per rewrite of the state in it.
type busyWindow struct {
	rewrites float64
	own, cpu []float64 // cores
	perCPU   []float64 // s per rewrite, its own
	perRead  []float64 // MiB per rewrite
	files    []string
}

func measureBusy(t *testing.T, procs []busyProcess, rewrites func() int64, win time.Duration) busyWindow {
	t.Helper()
	before := make([]busyUsage, len(procs))
	files := make([]map[string]int64, len(procs))
	for i, p := range procs {
		before[i], files[i] = p.usage(), p.files.snapshot()
	}
	first := rewrites()
	time.Sleep(win)
	w := busyWindow{rewrites: float64(rewrites() - first)}
	if w.rewrites < win.Seconds()/busyRewriteEvery.Seconds()/2 {
		t.Fatalf("%v rewrites in %s: the writer did not keep up", w.rewrites, win)
	}
	for i, p := range procs {
		after := p.usage()
		own := (after.own - before[i].own).Seconds()
		w.own = append(w.own, own/win.Seconds())
		w.cpu = append(w.cpu, (after.cpu-before[i].cpu).Seconds()/win.Seconds())
		w.perCPU = append(w.perCPU, own/w.rewrites)
		w.perRead = append(w.perRead, float64(after.read-before[i].read)/mib/w.rewrites)
		w.files = append(w.files, readsBetween(files[i], p.files.snapshot(), w.rewrites, "rewrite"))
	}
	return w
}

// TestBusyDeskBudgets runs the loop and the feeds beside a writer that rewrites
// the generated state once a second, and checks each one's own share of a core.
//
// control: none: the suite runs only in its own job (BEEKEEPER_PERF=1), so the
// merge base skips it here; the scenario busy-desk-guide-watch-reads-what-changed
// fails on the base
func TestBusyDeskBudgets(t *testing.T) {
	requirePerf(t)
	o := defaultDesk()
	d := newDesk(t, o)
	l := d.startLoop(t)
	procs := []busyProcess{{
		name: "supervisor loop", budget: busyLoopCPU, files: l.files,
		usage: func() busyUsage {
			u := l.usage(t)
			ps, _ := readStat(l.cmd.Process.Pid)
			return busyUsage{own: ps.own, cpu: u.cpu, read: u.read}
		},
	}}
	for _, w := range watchFeeds {
		f := d.startFeed(t, w.name, w.args)
		procs = append(procs, busyProcess{
			name: w.name, budget: busyFeedCPU, files: f.files,
			usage: func() busyUsage {
				cpu, read := f.usage(t)
				ps, _ := readStat(f.cmd.Process.Pid)
				return busyUsage{own: ps.own, cpu: cpu, read: read}
			},
		})
	}
	rewrites, stop := d.rewriteEvery(t, busyRewriteEvery)
	defer stop()
	// The loop's and the feeds' first reads take in everything once, the
	// loop's once-a-minute readers within a minute; budgets are for the
	// rewrites after.
	settle(o.loopInterval)
	win := loopWindow()
	w := measureBusy(t, procs, rewrites, win)
	rep.note("%s: %v rewrites of the state in %s (%s)", t.Name(), w.rewrites, win, runnerLoad())
	// A CPU over its budget is measured again over one more window and the
	// lower value counts, as the loop budgets do: a regression is over in
	// both, a loaded runner's slow moment in one.
	own := w.own
	for i, p := range procs {
		if p.budget.Max >= 0 && own[i] > p.budget.Max {
			again := measureBusy(t, procs, rewrites, win)
			rep.note("%s: measured again, %v rewrites (%s)", t.Name(), again.rewrites, runnerLoad())
			own = make([]float64, len(procs))
			for j := range procs {
				rep.note("%s %s, busy: %.3f of a core, again %.3f", t.Name(), procs[j].name, w.own[j], again.own[j])
				own[j] = min(w.own[j], again.own[j])
			}
			break
		}
	}
	for i, p := range procs {
		kase := p.name + ", busy"
		rep.check(t, kase, "core", own[i], p.budget)
		rep.check(t, kase+", with children", "core", w.cpu[i], measured)
		rep.check(t, kase+", per rewrite", "cpu", w.perCPU[i], measured)
		rep.check(t, kase+", per rewrite", "read", w.perRead[i], measured)
		rep.note("%s %s: %s", t.Name(), kase, w.files[i])
	}
}
