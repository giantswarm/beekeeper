package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) timerDispatchCmd() *cobra.Command {
	var once bool
	c := &cobra.Command{Use: "dispatch", Short: "Fire timers independently of the watch and supervisor", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return a.runTimerDispatcher(ctx, once)
		}}
	c.Flags().BoolVar(&once, "once", false, "settle one timer pass and wait for its actions")
	return c
}

// Only standalone dispatchers take this lock. Watch and supervisor keep using
// the same transactional claims, so upgrades with both running never double-fire.
func (a *app) runTimerDispatcher(ctx context.Context, once bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	l := flock.New(filepath.Join(a.store.Dir(), "timer-dispatch.lock"))
	ok, err := l.TryLock()
	if err != nil {
		return err
	}
	if !ok {
		return refused("another timer dispatcher runs")
	}
	defer func() { _ = l.Unlock() }()
	d := newTimerDispatchSession(ctx)
	defer d.close()
	tick := time.NewTicker(timerTick)
	defer tick.Stop()
	binary := loopBinary()
	for {
		if binary != nil && binary.Replaced() && d.active.Load() == 0 {
			return binary.Exec()
		}
		// Reload limits and paths without sharing mutable config with actions
		// dispatched by earlier ticks. A broken config fails this service and
		// remains visible to the independent fatal sentinel.
		cfg, err := config.Load(a.loadedCfg)
		if err != nil {
			return err
		}
		if cfg.StateDir != a.store.Dir() {
			return fmt.Errorf("timer state directory changed; reinstall the dispatcher")
		}
		current := *a
		current.cfg, current.now, current.loopNow = cfg, time.Now(), nil
		_, err = d.pass(&current)
		if err != nil {
			return err
		}
		if once {
			d.batches.Wait()
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// Each running epoch owns the contexts of all earlier batches. Stop drains
// them before a new epoch can dispatch the interrupted occurrences on resume.
type timerDispatchSession struct {
	parent  context.Context
	ctx     context.Context
	cancel  context.CancelFunc
	batches sync.WaitGroup
	active  atomic.Int32
}

func newTimerDispatchSession(ctx context.Context) *timerDispatchSession {
	d := &timerDispatchSession{parent: ctx}
	d.ctx, d.cancel = context.WithCancel(ctx)
	return d
}

func (d *timerDispatchSession) close() {
	d.cancel()
	d.batches.Wait()
}

func (d *timerDispatchSession) pass(a *app) (*watcher, error) {
	st, err := a.store.Read()
	if err != nil {
		return nil, err
	}
	if st.Stop != nil {
		d.close()
		d.ctx, d.cancel = context.WithCancel(d.parent)
		return &watcher{app: a.snapshotAtNow()}, nil
	}
	w, err := a.dispatchTimerPass(d.ctx)
	if err != nil {
		return nil, err
	}
	d.active.Add(1)
	d.batches.Go(func() { defer d.active.Add(-1); w.timerActs.Wait() })
	// Stop may have arrived while checks were running.
	st, err = a.store.Read()
	if err != nil {
		return nil, err
	}
	if st.Stop != nil {
		d.close()
		d.ctx, d.cancel = context.WithCancel(d.parent)
	}
	return w, nil
}

func (a *app) dispatchTimerPass(ctx context.Context) (*watcher, error) {
	st, err := a.store.Read()
	if err != nil {
		return nil, err
	}
	batch := a.snapshotAtNow()
	w := &watcher{app: batch}
	if st.Stop != nil {
		return w, nil
	}
	now := batch.now
	githubHeld := slices.ContainsFunc(st.Holds, func(h state.Hold) bool { return h.Target == holdGitHub && h.Active(now) })
	needsGitHub := slices.ContainsFunc(st.Timers, func(t state.Timer) bool { return checkOf(t).github && timerDispatchCheckDue(t, now) })
	if needsGitHub && !githubHeld {
		// A standalone driver must sample its own budget: with no watch or
		// loop, an unknown cached budget would otherwise defer CI wakes forever.
		pctx, cancel := context.WithTimeout(ctx, fatalProbeTimeout)
		_, probeErr := readBudget(batch, pctx)
		cancel()
		if probeErr != nil {
			githubHeld = true
		}
	}
	found := batch.loopTimerChecks(ctx, st.Timers, now, githubHeld || batch.resetFirst(st))
	// A GitHub admission refusal is an attempted check, not an absent tick.
	// Record it so overdue detection distinguishes a stopped scheduler from
	// an intentional budget/hold wait, without calling the reference healthy.
	for _, t := range st.Timers {
		if checkOf(t).github && timerDispatchCheckDue(t, now) {
			if _, ok := found[t.ID]; !ok {
				found[t.ID] = checkResult{unreadable: true, reason: "GitHub check deferred by admission"}
			}
		}
	}
	var lines []string
	var fires []timerFire
	err = a.store.Update(func(current *state.State) ([]state.Event, error) {
		if current.Stop != nil {
			return nil, nil
		}
		var events []state.Event
		lines, events, fires, _ = settleTimersMatching(current, found, now, timerTick, nil)
		return events, nil
	})
	if err != nil {
		return nil, err
	}
	for _, line := range lines {
		w.emitNow("timer", "%s", line)
	}
	w.actTimers(ctx, fires)
	return w, nil
}

func timerDispatchCheckDue(t state.Timer, now time.Time) bool {
	every := t.Every
	if every <= 0 {
		every = timerEvery
	}
	return t.Conditional() && !t.Due.After(now) && (t.Checked.IsZero() || now.Sub(t.Checked) >= every)
}

// A condition that is false but checked on schedule is not an outage. What
// matters is missing its next check (or final timeout), not its original due
// date. Explicit stop intent suppresses this alarm as well as dispatch.
func fatalTimers(st *state.State, now time.Time) fatalTimerFinding {
	if st.Stop != nil {
		return fatalTimerFinding{disabled: true}
	}
	n := 0
	for _, t := range st.Timers {
		if !t.Auto() || !t.Fired.IsZero() || t.Due.After(now) {
			continue
		}
		deadline := t.Due
		if t.Conditional() && !t.Checked.IsZero() {
			every := t.Every
			if every <= 0 {
				every = timerEvery
			}
			deadline = t.Checked.Add(every)
		}
		if !t.Until.IsZero() && t.Until.Before(deadline) {
			deadline = t.Until
		}
		if now.Sub(deadline) > 2*time.Minute {
			n++
		}
	}
	return fatalTimerFinding{overdue: n}
}

type fatalTimerFinding struct {
	overdue  int
	disabled bool
}

func (f fatalTimerFinding) evidence() string {
	return fmt.Sprintf("%d timer firing or condition-check deadlines missed by more than two minutes", f.overdue)
}
