package cmd

import (
	"context"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// recoveryOutput shares the parent watch's output lock without sharing its
// poll clock, process snapshot or successor chain.
type recoveryOutput struct{ watch *watcher }

func (o recoveryOutput) Write(p []byte) (int, error) {
	o.watch.mu.Lock()
	defer o.watch.mu.Unlock()
	return o.watch.out.Write(p)
}

func (w *watcher) newGuideRecovery() *watcher {
	a := *w.app
	a.out = recoveryOutput{w}
	g := a.newWatcher(true, false)
	g.clock = w.clock
	return g
}

const guideRecoveryKey = "guide-recovery"

// recoverGuide is independent of the main watch's network reads, event replay,
// accounting and maintenance. It needs only process identities and role state.
// Unknown process/state observations never mean that the guide is dead.
func (w *watcher) recoverGuide(ctx context.Context) {
	if ctx.Err() != nil || w.stand.starting.Load() || w.stand.busy.Load() {
		return
	}
	w.now = w.clk().Now()
	t, err := plat.Machine.Processes()
	if err != nil {
		w.fail(guideRecoveryKey, "cannot read guide processes: %v", err)
		return
	}
	w.table = t
	st, err := w.store.Peek()
	if err != nil {
		w.fail(guideRecoveryKey, "cannot read guide state: %v", err)
		return
	}
	w.clear(guideRecoveryKey)
	if st.Stop != nil {
		return
	}
	sessions := claude.DiscoverLiveness(w.cfg, t, w.now)
	w.resumeRestarted(ctx, guideRole, st, sessions)
	w.guideGone(ctx, st, sessions)
	for _, line := range w.daemonsOutOfUnits(liveDaemonsInUnits(t), proc.Cgroup, false, watchParty, w.now) {
		w.emit("guide-daemon "+line, "DOCTOR %s", line)
	}
}
