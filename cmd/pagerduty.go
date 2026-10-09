package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/central"
	"github.com/giantswarm/beekeeper/internal/config"
)

// pagerDutyReader reads alerts.pagerduty's services through muster's
// PagerDuty server as the person; run nil runs the muster binary.
func (a *app) pagerDutyReader(run central.Runner) alerts.PagerDutyReader {
	pd := a.cfg.Alerts.PagerDuty
	c := central.New(config.Central{Context: pd.Context, Server: pd.Server, Muster: a.cfg.Central.Muster, Timeout: a.cfg.Central.Timeout}, run)
	return alerts.PagerDutyReader{Services: pd.Services, Call: func(ctx context.Context, tool string, args map[string]any) (string, error) {
		text, err := c.Call(ctx, tool, args, nil)
		if err != nil {
			return text, fmt.Errorf("muster context %s, %s: %s", pd.Context, c.Tool(tool), unreadableReason(err))
		}
		return text, nil
	}}
}

// pagerDutyCycle reads the open incidents once, returns the lines of what
// changed and keeps the new baseline. An interrupted reading keeps nothing.
func (a *app) pagerDutyCycle(ctx context.Context, store *alerts.Store, run central.Runner) []string {
	st, err := store.Load()
	if err != nil {
		return []string{"PAGERDUTY baseline unreadable: " + err.Error()}
	}
	var known map[string]alerts.Incident
	if st.PagerDuty != nil {
		known = st.PagerDuty.Incidents
	}
	ans := a.pagerDutyReader(run).Read(ctx, known)
	if ctx.Err() != nil {
		return nil
	}
	lines, next := a.alertRules().PagerDutyStep(st.PagerDuty, ans, time.Now())
	st.PagerDuty = next
	if err := store.Save(st); err != nil {
		lines = append(lines, "PAGERDUTY baseline not saved: "+err.Error())
	}
	return lines
}

// pagerDutySnapshot is the open incidents, none when PagerDuty is not read.
func (a *app) pagerDutySnapshot(ctx context.Context) []string {
	if !a.cfg.Alerts.PagerDuty.Enabled() {
		return nil
	}
	return a.alertRules().PagerDutySnapshot(a.pagerDutyReader(nil).Read(ctx, nil), time.Now())
}

// ownPagerDuty makes the caller the owner of the incidents' baseline, or
// says which watch is.
func (a *app) ownPagerDuty() (*alerts.Store, error) {
	store := alerts.NewPagerDutyStore(a.cfg.StateDir)
	owned, owner, err := store.Own()
	switch {
	case err != nil:
		return nil, err
	case !owned:
		return nil, errors.New(ownedBy("the PagerDuty incidents", owner, a.now))
	}
	return store, nil
}

func ownedBy(what string, owner *alerts.Owner, now time.Time) string {
	return fmt.Sprintf("%s are read by pid %d (a beekeeper watch) since %s", what, owner.PID, clock(now, owner.Since))
}

// watchPagerDuty reads the open incidents every alerts.pagerduty.every
// while this watch owns their baseline, at the same pace on a strained
// machine: a reading is two muster calls, and a page waits for none.
func (w *watcher) watchPagerDuty(ctx context.Context) {
	store := alerts.NewPagerDutyStore(w.cfg.StateDir)
	defer func() { _ = store.Release() }()
	other := 0
	w.loop(ctx, w.cfg.Alerts.PagerDuty.Every.Duration, false, func(ctx context.Context) {
		owned, owner, err := store.Own()
		switch {
		case err != nil:
			w.emit("pagerduty", "PAGERDUTY baseline unusable: %v", err)
		case !owned:
			w.clear("pagerduty")
			if owner.PID != other {
				other = owner.PID
				w.emitNow("pagerduty", "PAGERDUTY read by the watch with pid %d; this one takes over when it ends", other)
			}
		default:
			w.clear("pagerduty")
			if other != 0 {
				other = 0
				w.emitNow("pagerduty", "PAGERDUTY taken over by this watch")
			}
			for _, l := range w.pagerDutyCycle(ctx, store, w.musterRun) {
				w.emitNow("pagerduty", "%s", l)
			}
		}
	})
}
