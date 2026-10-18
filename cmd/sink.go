package cmd

import (
	"context"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/sink"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The verbs of the events the sink may dispatch, besides verbMerged.
const (
	verbServe        = config.SinkServe
	verbBoardClaim   = config.SinkBoardClaim
	verbPlanGrilling = config.SinkPlanGrilling
	verbPlanReady    = config.SinkPlanReady
)

// sinkGH is the gh CLI the sink dispatches through; a seam for tests.
var sinkGH github.GH = github.RunGH

// sinkKey is the watch's condition of a failed dispatch.
const sinkKey = "sink"

// sink offers the events of the sink's verbs since the last poll, and those
// still within sink.Window, to the ledger, and dispatches the workflow for
// each one it claims, in the background: a watch that keeps running (not
// --once) dispatches them within one watch.interval. Each dispatch is
// logged (sink.dispatched); a failed one is said (SINK FAILED, sink.failed)
// and given back, so the next poll tries it again within the window.
func (w *watcher) sink(ctx context.Context) {
	s := w.cfg.Sink
	if !s.Enabled() || !w.chores {
		return
	}
	if w.sinkLedger == nil {
		// Started now, the ledger dispatches what is logged from now on.
		l := sink.NewLedger(w.store.Dir())
		if _, err := l.Claim(ctx, w.now, nil); err != nil {
			w.fail(sinkKey, "SINK FAILED: cannot start its ledger: %v", err)
			return
		}
		w.sinkLedger = l
	}
	evs, err := w.store.Follow("watch.sink", func(e state.Event) bool { return e.Subject != "" && s.Selects(e.Verb) })
	if err != nil {
		return // the next poll reads them again
	}
	w.sinkPending = slices.DeleteFunc(append(w.sinkPending, evs...), func(e state.Event) bool { return w.now.Sub(e.At) > sink.Window })
	if len(w.sinkPending) == 0 {
		return
	}
	pending := slices.Clone(w.sinkPending)
	w.inFlight(ctx, 0, &w.sinking, func(ctx context.Context) { w.dispatch(ctx, s, pending) })
}

// dispatch claims pending in the ledger and dispatches the workflow for
// each event it claimed.
func (w *watcher) dispatch(ctx context.Context, s config.Sink, pending []state.Event) {
	now := time.Now()
	evs, err := w.sinkLedger.Claim(ctx, now, pending)
	if err != nil {
		w.fail(sinkKey, "SINK FAILED: cannot claim the events to dispatch: %v", err)
		return
	}
	failed := false
	for _, e := range evs {
		if err := sink.Dispatch(ctx, sinkGH, s, e); err != nil {
			failed = true
			_ = w.sinkLedger.Unclaim(ctx, now, e)
			w.fail(sinkKey, "SINK FAILED: %s %s: %v", e.Verb, e.Subject, err)
			_ = w.store.Log(event(watchParty, "sink.failed", "%s %s: %v", e.Verb, e.Subject, err))
			continue
		}
		_ = w.store.Log(event(watchParty, "sink.dispatched", "%s %s: %s %s", e.Verb, e.Subject, s.Repo, s.Workflow))
	}
	if !failed {
		w.clear(sinkKey)
	}
}
