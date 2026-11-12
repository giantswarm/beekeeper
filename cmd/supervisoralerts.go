package cmd

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// readLoopAlerts consumes the watch's durable snapshot. Only watch reads
// installations; a loop restart can still route a page it has not handled.
var readLoopAlerts = func(_ context.Context, a *app) ([]alerts.Firing, error) {
	var current []alerts.Firing
	if at, err := a.readWatchLevel("alert-state", &current); err != nil {
		return nil, err
	} else if !at.IsZero() {
		return current, nil
	}
	// Followed, not searched: without a watch the event may never have been
	// logged, and every pass would read the whole history again.
	evs, err := state.FollowLast(a.store, "watch-alert-state", 1, func(e state.Event) bool { return e.Verb == "watch.alert-state" })
	if err != nil || len(evs) == 0 {
		return nil, err
	}
	var firing []alerts.Firing
	err = json.Unmarshal([]byte(evs[len(evs)-1].Detail), &firing)
	return firing, err
}

// Only a proven unresumable session is replaced. Capacity refusals and
// transient wake failures leave the worker and its task on the roster.
var loopResumable = func(a *app, st *state.State, ag state.Agent) bool {
	if ag.Execution != nil {
		return true
	}
	if _, _, ok := harnessForHost(ag.HostSession); ok {
		sessions, _, err := a.sessions()
		if err != nil {
			return true
		} // an unreadable source proves no session ended
		return slices.ContainsFunc(sessions, func(s *claude.Session) bool { return ag.Is(s.Party()) })
	}
	_, err := resolveWake(a.cfg, st, ag)
	return err == nil
}
