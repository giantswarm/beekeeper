package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// startRefillCadence gives refill its own clock. It never overlaps itself,
// and shutdown joins it before the loop store and action streams are closed.
func (l *supervisorLoop) startRefillCadence(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	interval := l.a.cfg.Supervisor.LoopInterval.Duration
	if interval <= 0 {
		interval = time.Second
	}
	ticks := time.NewTicker(interval)
	go func() {
		defer close(done)
		defer ticks.Stop()
		l.runRefillCadence(ctx, ticks.C)
	}()
	return func() {
		cancel()
		<-done
	}
}

func (l *supervisorLoop) runRefillCadence(ctx context.Context, ticks <-chan time.Time) {
	// Refill immediately on startup; subsequent ticks retain the configured cadence.
	for {
		if ctx.Err() != nil {
			return
		}
		actCtx, cancel := context.WithCancel(l.backgroundContext(ctx))
		stop := context.AfterFunc(ctx, cancel)
		err := l.refillTick(actCtx)
		stop()
		cancel()
		if err != nil {
			l.sayOnce("refill cadence", "SUPERVISOR REFILL FAILED: %v", err)
		} else {
			l.clearSaid("refill cadence")
		}
		select {
		case <-ctx.Done():
			return
		case now, ok := <-ticks:
			if !ok {
				return
			}
			if l.a.loopNow != nil {
				l.a.loopNow.Store(&now)
			}
		}
	}
}

func (l *supervisorLoop) refillTick(ctx context.Context) error {
	// Configuration is stable for the entire dispatch, including its child commands.
	l.refillConfigMu.RLock()
	defer l.refillConfigMu.RUnlock()
	if ctx.Err() != nil || l.pauseForReexec() || !l.beginInFlight(loopFillStream) {
		return nil
	}
	defer l.endInFlight(loopFillStream)
	if !l.dryRun {
		if err := l.refreshStartConditions(ctx); err != nil {
			l.holdStarts(true)
			return err
		}
	}
	if l.a.cfg.Supervisor.WorkerBrief == "" {
		return nil
	}
	if l.holdingStarts() {
		l.fillRan()
		l.stallClear()
		return nil
	}
	l.setFillPhase("read state")
	defer l.setFillPhase("")
	st, err := l.a.store.Read()
	if err != nil {
		return err
	}
	l.setFillPhase("read refill history")
	// Use a separate cursor from observation; both can advance concurrently.
	history, err := state.FollowSnapshot(l.a.store, "supervisor-refill-taken", "v1",
		func(e state.Event) bool { return strings.HasPrefix(e.Verb, "supervisor.") })
	if err != nil {
		return err
	}
	folded := l.refillFold.fold(history)
	for _, taken := range folded.taken {
		delete(folded.claims, taken.Key)
	}
	reservations, _ := l.deliveryReservations(st)
	l.mu.Lock()
	l.boardNone = nil
	l.mu.Unlock()
	return l.fill(ctx, st, folded.claims, folded.taken, reservations)
}

func (l *supervisorLoop) setFillPhase(phase string) {
	l.mu.Lock()
	l.fillPhase = phase
	l.mu.Unlock()
}

func (l *supervisorLoop) refillBlocker() string {
	l.mu.Lock()
	phase := l.fillPhase
	active := l.inFlight[loopFillStream]
	l.mu.Unlock()
	if phase != "" {
		return fmt.Sprintf("refill action %q is still running", phase)
	}
	if active {
		return "a previous capacity refill is still running"
	}
	return "no refill is running; dispatch has not reached the capacity refill"
}
