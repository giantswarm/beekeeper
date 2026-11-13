package loop

import (
	"context"
	"fmt"
	"strings"
	"time"

	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/state"
)

// startRefillCadence gives refill its own clock. It never overlaps itself,
// and shutdown joins it before the loop store and action streams are closed.
func (l *Loop) startRefillCadence(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	interval := l.Host.Cfg().Supervisor.LoopInterval.Duration
	if interval <= 0 {
		interval = time.Second
	}
	ticks := clk.NewTicker(interval)
	go func() {
		defer close(done)
		defer ticks.Stop()
		l.RunRefillCadence(ctx, ticks.C)
	}()
	return func() {
		cancel()
		<-done
	}
}

func (l *Loop) RunRefillCadence(ctx context.Context, ticks <-chan time.Time) {
	// Refill immediately on startup; subsequent ticks retain the configured cadence.
	for {
		if ctx.Err() != nil {
			return
		}
		actCtx, cancel := context.WithCancel(l.BackgroundContext(ctx))
		stop := context.AfterFunc(ctx, cancel)
		err := l.RefillTick(actCtx)
		stop()
		cancel()
		if err != nil {
			l.SayOnce("refill cadence", "SUPERVISOR REFILL FAILED: %v", err)
		} else {
			l.ClearSaid("refill cadence")
		}
		select {
		case <-ctx.Done():
			return
		case now, ok := <-ticks:
			if !ok {
				return
			}
			if l.Host.LoopNow() != nil {
				l.Host.LoopNow().Store(&now)
			}
		}
	}
}

func (l *Loop) RefillTick(ctx context.Context) (result error) {
	// Configuration is stable for the entire dispatch, including its child commands.
	l.RefillConfigMu.RLock()
	defer l.RefillConfigMu.RUnlock()
	dispatched := false
	why := "loop stopping or waiting to re-exec"
	defer func() {
		if !l.DryRun && !dispatched && l.Host.Cfg().Supervisor.WorkerBrief != "" {
			if result != nil {
				why = Cmd.OneLine(result.Error())
			}
			_, _ = fmt.Fprintf(l.Host.Out(), RefillZeroFormat, why)
		}
	}()
	if ctx.Err() != nil || l.PauseForReexec() {
		return nil
	}
	if !l.BeginInFlight(LoopFillStream) {
		why = "previous refill still running"
		return nil
	}
	defer l.EndInFlight(LoopFillStream)
	why = "starts held"
	if !l.DryRun {
		reason, err := l.refreshStartConditions(ctx)
		if err != nil {
			l.HoldStarts(true)
			return err
		}
		if reason != "" {
			why = reason
		}
	}
	if l.Host.Cfg().Supervisor.WorkerBrief == "" {
		return nil
	}
	if l.HoldingStarts() {
		l.FillRan()
		l.StallClear()
		return nil
	}
	l.SetFillPhase("read state")
	defer l.SetFillPhase("")
	st, err := l.Host.Store().Read()
	if err != nil {
		return err
	}
	l.SetFillPhase("read refill history")
	// Use a separate cursor from observation; both can advance concurrently.
	history, err := state.FollowSnapshot(l.Host.Store(), "supervisor-refill-taken", "v1",
		func(e state.Event) bool { return strings.HasPrefix(e.Verb, "supervisor.") })
	if err != nil {
		return err
	}
	folded := l.refillFold.Fold(history)
	for _, taken := range folded.Taken {
		delete(folded.Claims, taken.Key)
	}
	reservations, _ := l.DeliveryReservations(st)
	l.Mu.Lock()
	l.BoardNone = nil
	l.Mu.Unlock()
	dispatched = true // Fill owns the summary once it can attempt starts.
	return l.Fill(ctx, st, folded.Claims, folded.Taken, reservations)
}

func (l *Loop) SetFillPhase(phase string) {
	l.Mu.Lock()
	l.fillPhase = phase
	l.Mu.Unlock()
}

func (l *Loop) refillBlocker() string {
	l.Mu.Lock()
	phase := l.fillPhase
	active := l.inFlight[LoopFillStream]
	l.Mu.Unlock()
	if phase != "" {
		return fmt.Sprintf("refill action %q is still running", phase)
	}
	if active {
		return "a previous capacity refill is still running"
	}
	return "no refill is running; dispatch has not reached the capacity refill"
}
