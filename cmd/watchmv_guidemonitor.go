package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
)

// GuideMonitor runs in the desk watch, outside the guide's Monitor lifetime.
// Reports become owned state before notification, so a dead CLI loses no work.
func (w *Watcher) GuideMonitor(ctx context.Context, st *state.State, sessions []*claude.Session) {
	if st.Stop != nil || st.Guide == nil || st.Guide.Holder == nil {
		return
	}
	events, err := w.Host.Store().Follow("independent-guide-reports", func(e state.Event) bool { return e.Verb == loop.LoopFileVerb })
	if err != nil {
		w.Fail("guide-monitor", "cannot read guide reports: %v", err)
		return
	}
	var keys, lines []string
	var to string
	var holder state.Party
	step := func(st *state.State) bool {
		keys, lines = nil, nil
		r := st.Guide
		if st.Stop != nil || r == nil || r.Holder == nil {
			return false
		}
		to = r.Holder.Name
		holder = r.Holder.Party

		reportsSince, workText, reportCount, deliveredBefore := st.GuideReportsSince, Cmd.GuideWorkText(st), len(st.GuideReports), len(r.Delivered)
		var current []string
		notice := func(key, text string) {
			key = r.Holder.Session + ":" + r.Holder.HostSession + ":" + key
			current = append(current, key)
			if !slices.Contains(r.Delivered, key) {
				keys = append(keys, key)
				lines = append(lines, text)
			}
		}
		since := r.WatchSeen
		if since.IsZero() {
			since = r.Holder.Since
		}
		grace := max(2*time.Minute, 3*w.Host.Cfg().Watch.Interval.Duration)
		// Both consumers retain reports under the same lock; the first owns
		// delivery and the other observes the durable report key.
		for _, e := range Cmd.GuideReportEvents(st, events, w.Host.Now()) {
			key, text, ok := loop.Keyed(e.Detail)
			if !ok || !strings.Contains(text, loop.FileReportSeparator) {
				continue
			}
			Cmd.RetainGuideReport(st, key, text, e.At, r.Holder.Party)
		}

		if !since.IsZero() && w.Host.Now().Sub(since) > grace {
			notice("watch:"+since.String(), "GUIDE WATCH LAPSED: the independent desk watch has seen no guide feed heartbeat since "+since.Format(time.RFC3339)+"; re-arm `beekeeper guide watch` and read `beekeeper guide work`.")
		}
		for _, n := range st.Notes {
			if Cmd.Guides(w.Host.Cfg().Guide.Person, &n) && !slices.Contains(r.Fed, fmt.Sprintf("note#%d", n.ID)) {
				notice(fmt.Sprintf("note:%d:%s", n.ID, n.At.String()), fmt.Sprintf("GUIDE DECISION: note %d is waiting; run `beekeeper guide next` and ask in conversation without a blocking tool.", n.ID))
			}
		}
		for _, work := range st.GuideWork {
			if Cmd.GuideWorkBlocks(work, w.Host.Now()) && !slices.Contains(r.Fed, "problem:"+work.Source) {
				notice(fmt.Sprintf("work:%d:%s", work.ID, work.Due.String()), fmt.Sprintf("GUIDE WORK DUE: %d [%s] %s; run `beekeeper guide work`.", work.ID, work.Kind, Cmd.EscapedLine(work.Text)))
			}
		}
		r.Delivered = slices.DeleteFunc(r.Delivered, func(key string) bool { return !slices.Contains(current, key) })
		return !reportsSince.Equal(st.GuideReportsSince) || workText != Cmd.GuideWorkText(st) || reportCount != len(st.GuideReports) || len(r.Delivered) != deliveredBefore
	}
	if step(st) {
		err = w.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
			step(st)
			return nil, nil
		})
		if err != nil {
			w.Fail("guide-monitor", "cannot retain guide work: %v", err)
			return
		}
	}
	w.Clear("guide-monitor")
	if len(lines) == 0 {
		return
	}
	// The watch line is independent of notification and remains visible even
	// if the guide cannot be reached. Failed notifications retry next tick.
	for _, line := range lines {
		w.EmitNow("guide-monitor", "%s", line)
	}
	_, live := claude.Live(sessions, holder)
	if w.Stand.Send == nil || !live {
		return
	}
	w.SendAsync(ctx, to, strings.Join(lines, "\n"), func(err error) {
		if err != nil {
			_ = w.Host.Store().Record(state.Event{At: time.Now(), By: WatchParty, Verb: "guide.delivery.failed", Detail: err.Error()})
			return
		}
		if err := w.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
			if st.Guide != nil && st.Guide.Holder != nil && st.Guide.Holder.Is(holder) {
				for _, key := range keys {
					if !slices.Contains(st.Guide.Delivered, key) {
						st.Guide.Delivered = append(st.Guide.Delivered, key)
					}
				}
			}
			return nil, nil
		}); err != nil {
			_ = w.Host.Store().Record(state.Event{At: time.Now(), By: WatchParty, Verb: "guide.delivery.failed", Detail: err.Error()})
		}
	})
}
