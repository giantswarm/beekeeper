package watch

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/notify"
	"github.com/giantswarm/beekeeper/internal/state"
)

// pageKey starts the reported key of a PAGE UNOWNED line.
const pageKey = "page "

// OwnedUntil tells of an alert's key when its last owner stopped owning it:
// zero while a person or a running session owns it, the session's end else
// (gone is now for an owner gone with no end recorded yet), false when it
// never had an owner.
func OwnedUntil(owners []state.AlertOwner, sessions []*claude.Session, now time.Time) func(string) (time.Time, bool) {
	return func(key string) (time.Time, bool) {
		var until time.Time
		found := false
		for _, o := range owners {
			if Cmd.AlertOwnerKey(o) != key {
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
		gone := !slices.ContainsFunc(firing, func(f alerts.Firing) bool { return f.Key == Cmd.AlertOwnerKey(o) })
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

// UnownedPages says PAGE UNOWNED for each firing page of the alert baseline,
// alerts.team's when it is set, that has had no owner for alerts.ownerGrace, once per grace period while it
// stays unowned, and notifies (page-unowned). A watch that keeps a mark
// records the ends of the owners whose session is gone; --once writes
// nothing and counts such an owner's page from now.
func (w *Watcher) UnownedPages(ctx context.Context, sessions []*claude.Session) {
	al := w.Host.Cfg().Alerts
	base, err := alerts.NewStore(w.Host.Cfg().StateDir).Load()
	if err != nil {
		return
	}
	pages := slices.DeleteFunc(base.Firing(al.PageSeverity), func(f alerts.Firing) bool { return al.Team != "" && f.Team != al.Team })
	var due []alerts.Unowned
	if len(pages) > 0 {
		st, err := w.Host.Store().Peek()
		if err != nil {
			return
		}
		owners := st.AlertOwners
		if w.Chores && len(owners) > 0 {
			if next, changed := endedOwners(owners, base.Firing(""), sessions, w.Host.Now()); changed {
				err := w.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
					st.AlertOwners, _ = endedOwners(st.AlertOwners, base.Firing(""), sessions, w.Host.Now())
					return nil, nil
				})
				if err == nil {
					owners = next
				}
			}
		}
		due = alerts.UnownedPages(pages, al.OwnerGrace.Duration, OwnedUntil(owners, sessions, w.Host.Now()), w.Host.Now())
	}
	current := map[string]bool{}
	for _, u := range due {
		k := pageKey + u.Mark
		current[k] = true
		if w.Reported[k] {
			continue
		}
		w.Reported[k], w.Dirty = true, true
		l := u.Line()
		w.EmitNow("alerts", "%s", l)
		w.Notify(ctx, notify.PageUnowned, u.Mark, fmt.Sprintf("beekeeper: page unowned on %s for %s", u.Installation, Cmd.Dur(u.For.Round(time.Minute))), l)
	}
	for k := range w.Reported {
		if strings.HasPrefix(k, pageKey) && !current[k] {
			delete(w.Reported, k)
			w.Dirty = true
		}
	}
}
