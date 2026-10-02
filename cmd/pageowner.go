package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/notify"
	"github.com/giantswarm/beekeeper/internal/state"
)

// verbAlertOwned is the event of alerts own.
const verbAlertOwned = "alert.owned"

// pageKey starts the reported key of a PAGE UNOWNED line.
const pageKey = "page "

func (a *app) alertsOwnCmd() *cobra.Command {
	var by string
	c := &cobra.Command{
		Use:   "own <installation/alertname | alertname | fingerprint>",
		Short: "Record the session that owns a firing alert, so the watch stops saying PAGE UNOWNED",
		Long: `Record the caller (--by: another running session) as the owner of the
firing alerts the argument names in the alert baseline: <installation>/<alertname>,
an alertname on any installation, or an alert's fingerprint. Each alert has
one owner; a new one takes it over. Ownership ends when the owning session
ends or the alert resolves; an alert that fires again is a new one. A person
(--as) owns an alert until it resolves.

The watch says PAGE UNOWNED for a firing alert at alerts.pageSeverity
(default page), of alerts.team when it is set, that has had no owner for alerts.ownerGrace (15m), and again
every alerts.ownerGrace while it stays unowned; --notify sends it to the
desktop (page-unowned, critical). The owner's session ending starts the
count again.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			owner, err := a.alertOwnerParty(by)
			if err != nil {
				return err
			}
			base, err := alerts.NewStore(a.cfg.StateDir).Load()
			if err != nil {
				return err
			}
			firing := base.Firing("")
			hits := alerts.Match(firing, args[0])
			if len(hits) == 0 {
				return noAlertMatches(args[0], base.Firing(a.cfg.Alerts.PageSeverity))
			}
			now := a.now.UTC()
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.AlertOwners = ownAlerts(st.AlertOwners, firing, hits, owner, now)
				evs := make([]state.Event, len(hits))
				for i, f := range hits {
					evs[i] = event(owner, verbAlertOwned, "%s %s since %s", f.Name(), f.Where, f.Since)
				}
				return evs, nil
			}); err != nil {
				return err
			}
			for _, f := range hits {
				_, _ = fmt.Fprintf(a.out, "%q owns %s %s %s %s\n", owner.Name, f.Installation, f.Severity, f.Alertname, f.Where)
			}
			return nil
		},
	}
	c.Flags().StringVar(&by, "by", "", "the running session that owns it (name or id; default the caller)")
	return c
}

// alertOwnerParty is the running session by names, else the caller.
func (a *app) alertOwnerParty(by string) (state.Party, error) {
	if by == "" {
		return a.caller()
	}
	t, err := plat.Machine.Processes()
	if err != nil {
		return state.Party{}, err
	}
	s, err := claude.Resolve(claude.Discover(a.cfg, t, a.now), by)
	if err != nil {
		return state.Party{}, err
	}
	return s.Party(), nil
}

// noAlertMatches says that q names no firing alert, with the firing pages.
func noAlertMatches(q string, pages []alerts.Firing) error {
	if len(pages) == 0 {
		return fmt.Errorf("no firing alert matches %q, and no page fires", q)
	}
	names := make([]string, 0, len(pages))
	for _, p := range pages {
		if !slices.Contains(names, p.Name()) {
			names = append(names, p.Name())
		}
	}
	return fmt.Errorf("no firing alert matches %q; firing pages: %s", q, strings.Join(names, ", "))
}

// ownAlerts are the owners with hits owned by owner at now: a new owner
// takes an alert over, and the owners of alerts no longer firing are gone.
func ownAlerts(owners []state.AlertOwner, firing, hits []alerts.Firing, owner state.Party, now time.Time) []state.AlertOwner {
	keep := func(key string) bool {
		return slices.ContainsFunc(firing, func(f alerts.Firing) bool { return f.Key == key }) &&
			!slices.ContainsFunc(hits, func(f alerts.Firing) bool { return f.Key == key })
	}
	out := slices.DeleteFunc(slices.Clone(owners), func(o state.AlertOwner) bool { return !keep(o.Alert) })
	for _, f := range hits {
		out = append(out, state.AlertOwner{Alert: f.Key, Name: f.Name(), By: owner, At: now})
	}
	return out
}

// ownedUntil tells of an alert's key when its last owner stopped owning it:
// zero while a person or a running session owns it, the session's end else
// (gone is now for an owner gone with no end recorded yet), false when it
// never had an owner.
func ownedUntil(owners []state.AlertOwner, sessions []*claude.Session, now time.Time) func(string) (time.Time, bool) {
	return func(key string) (time.Time, bool) {
		var until time.Time
		found := false
		for _, o := range owners {
			if o.Alert != key {
				continue
			}
			found = true
			end := o.Ended
			if end.IsZero() {
				if _, live := claude.Live(sessions, o.By); live || o.Person() {
					return time.Time{}, true
				}
				end = now
			}
			if end.After(until) {
				until = end
			}
		}
		return until, found
	}
}

// endedOwners records the end of every owner whose session is gone, and
// drops the owners of alerts no longer firing; it reports whether anything
// changed.
func endedOwners(owners []state.AlertOwner, firing []alerts.Firing, sessions []*claude.Session, now time.Time) ([]state.AlertOwner, bool) {
	changed := false
	out := slices.DeleteFunc(slices.Clone(owners), func(o state.AlertOwner) bool {
		gone := !slices.ContainsFunc(firing, func(f alerts.Firing) bool { return f.Key == o.Alert })
		changed = changed || gone
		return gone
	})
	for i, o := range out {
		if _, live := claude.Live(sessions, o.By); o.Ended.IsZero() && !live && !o.Person() {
			out[i].Ended, changed = now, true
		}
	}
	return out, changed
}

// unownedPages says PAGE UNOWNED for each firing page of the alert baseline,
// alerts.team's when it is set, that has had no owner for alerts.ownerGrace, once per grace period while it
// stays unowned, and notifies (page-unowned). A watch that keeps a mark
// records the ends of the owners whose session is gone; --once writes
// nothing and counts such an owner's page from now.
func (w *watcher) unownedPages(ctx context.Context, sessions []*claude.Session) {
	al := w.cfg.Alerts
	base, err := alerts.NewStore(w.cfg.StateDir).Load()
	if err != nil {
		return
	}
	pages := slices.DeleteFunc(base.Firing(al.PageSeverity), func(f alerts.Firing) bool { return al.Team != "" && f.Team != al.Team })
	var due []alerts.Unowned
	if len(pages) > 0 {
		st, err := w.store.Read()
		if err != nil {
			return
		}
		owners := st.AlertOwners
		if w.chores && len(owners) > 0 {
			if next, changed := endedOwners(owners, base.Firing(""), sessions, w.now); changed {
				err := w.store.Update(func(st *state.State) ([]state.Event, error) {
					st.AlertOwners, _ = endedOwners(st.AlertOwners, base.Firing(""), sessions, w.now)
					return nil, nil
				})
				if err == nil {
					owners = next
				}
			}
		}
		due = alerts.UnownedPages(pages, al.OwnerGrace.Duration, ownedUntil(owners, sessions, w.now), w.now)
	}
	current := map[string]bool{}
	for _, u := range due {
		k := pageKey + u.Mark
		current[k] = true
		if w.reported[k] {
			continue
		}
		w.reported[k], w.dirty = true, true
		l := u.Line()
		w.emitNow("alerts", "%s", l)
		w.notify(ctx, notify.PageUnowned, u.Mark, fmt.Sprintf("beekeeper: page unowned on %s for %s", u.Installation, dur(u.For.Round(time.Minute))), l)
	}
	for k := range w.reported {
		if strings.HasPrefix(k, pageKey) && !current[k] {
			delete(w.reported, k)
			w.dirty = true
		}
	}
}
