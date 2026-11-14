package cmd

import (
	"context"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// recoveryOutput shares the parent watch's output lock without sharing its
// poll clock, process snapshot or successor chain.
type recoveryOutput struct{ watch *Watcher }

func (o recoveryOutput) Write(p []byte) (int, error) {
	o.watch.Mu.Lock()
	defer o.watch.Mu.Unlock()
	return o.watch.Host.out.Write(p)
}

func watchNewGuideRecovery(w *Watcher) *Watcher {
	a := *w.Host
	a.out = recoveryOutput{w}
	g := a.newWatcher(true, false)
	g.Clock = w.Clock
	return g
}

const guideRecoveryKey = "guide-recovery"

// watchRecoverGuide is independent of the main watch's network reads, event replay,
// accounting and maintenance. It needs only process identities and role state.
// Unknown process/state observations never mean that the guide is dead.
func watchRecoverGuide(w *Watcher, ctx context.Context) {
	if ctx.Err() != nil || w.Stand.Starting.Load() || w.Stand.Busy.Load() {
		return
	}
	w.Host.now = w.Clk().Now()
	t, err := plat.Machine.Processes()
	if err != nil {
		w.Fail(guideRecoveryKey, "cannot read guide processes: %v", err)
		return
	}
	w.Table = t
	st, err := w.Host.store.Peek()
	if err != nil {
		w.Fail(guideRecoveryKey, "cannot read guide state: %v", err)
		return
	}
	w.Clear(guideRecoveryKey)
	if st.Stop != nil {
		return
	}
	sessions := claude.DiscoverLiveness(w.Host.cfg, t, w.Host.now)
	w.ResumeRestarted(ctx, guideRole, st, sessions)
	w.GuideGone(ctx, st, sessions)
	for _, line := range w.Host.daemonsOutOfUnits(liveDaemonsInUnits(t), proc.Cgroup, false, WatchParty, w.Host.now) {
		w.Emit("guide-daemon "+line, "DOCTOR %s", line)
	}
}
