package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// guideMonitor runs in the desk watch, outside the guide's Monitor lifetime.
// Reports become owned state before notification, so a dead CLI loses no work.
func (w *watcher) guideMonitor(ctx context.Context, st *state.State, sessions []*claude.Session) {
	if st.Stop != nil || st.Guide == nil || st.Guide.Holder == nil {
		return
	}
	events, err := w.store.Follow("independent-guide-reports", func(e state.Event) bool { return e.Verb == LoopFileVerb })
	if err != nil {
		w.fail("guide-monitor", "cannot read guide reports: %v", err)
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

		reportsSince, workText, reportCount, deliveredBefore := st.GuideReportsSince, guideWorkText(st), len(st.GuideReports), len(r.Delivered)
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
		grace := max(2*time.Minute, 3*w.cfg.Watch.Interval.Duration)
		// Both consumers retain reports under the same lock; the first owns
		// delivery and the other observes the durable report key.
		for _, e := range guideReportEvents(st, events, w.now) {
			key, text, ok := Keyed(e.Detail)
			if !ok || !strings.Contains(text, FileReportSeparator) {
				continue
			}
			retainGuideReport(st, key, text, e.At, r.Holder.Party)
		}

		if !since.IsZero() && w.now.Sub(since) > grace {
			notice("watch:"+since.String(), "GUIDE WATCH LAPSED: the independent desk watch has seen no guide feed heartbeat since "+since.Format(time.RFC3339)+"; re-arm `beekeeper guide watch` and read `beekeeper guide work`.")
		}
		for _, n := range st.Notes {
			if guides(w.cfg.Guide.Person, &n) && !slices.Contains(r.Fed, fmt.Sprintf("note#%d", n.ID)) {
				notice(fmt.Sprintf("note:%d:%s", n.ID, n.At.String()), fmt.Sprintf("GUIDE DECISION: note %d is waiting; run `beekeeper guide next` and ask in conversation without a blocking tool.", n.ID))
			}
		}
		for _, work := range st.GuideWork {
			if guideWorkBlocks(work, w.now) && !slices.Contains(r.Fed, "problem:"+work.Source) {
				notice(fmt.Sprintf("work:%d:%s", work.ID, work.Due.String()), fmt.Sprintf("GUIDE WORK DUE: %d [%s] %s; run `beekeeper guide work`.", work.ID, work.Kind, escapedLine(work.Text)))
			}
		}
		r.Delivered = slices.DeleteFunc(r.Delivered, func(key string) bool { return !slices.Contains(current, key) })
		return !reportsSince.Equal(st.GuideReportsSince) || workText != guideWorkText(st) || reportCount != len(st.GuideReports) || len(r.Delivered) != deliveredBefore
	}
	if step(st) {
		err = w.store.Update(func(st *state.State) ([]state.Event, error) {
			step(st)
			return nil, nil
		})
		if err != nil {
			w.fail("guide-monitor", "cannot retain guide work: %v", err)
			return
		}
	}
	w.clear("guide-monitor")
	if len(lines) == 0 {
		return
	}
	// The watch line is independent of notification and remains visible even
	// if the guide cannot be reached. Failed notifications retry next tick.
	for _, line := range lines {
		w.emitNow("guide-monitor", "%s", line)
	}
	_, live := claude.Live(sessions, holder)
	if w.stand.send == nil || !live {
		return
	}
	w.sendAsync(ctx, to, strings.Join(lines, "\n"), func(err error) {
		if err != nil {
			_ = w.store.Record(state.Event{At: time.Now(), By: watchParty, Verb: "guide.delivery.failed", Detail: err.Error()})
			return
		}
		if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
			if st.Guide != nil && st.Guide.Holder != nil && st.Guide.Holder.Is(holder) {
				for _, key := range keys {
					if !slices.Contains(st.Guide.Delivered, key) {
						st.Guide.Delivered = append(st.Guide.Delivered, key)
					}
				}
			}
			return nil, nil
		}); err != nil {
			_ = w.store.Record(state.Event{At: time.Now(), By: watchParty, Verb: "guide.delivery.failed", Detail: err.Error()})
		}
	})
}

// guideStopReason flags actionable work at a turn boundary. Future work has
// an owner and deadline in state and therefore survives the turn and relay.
func guideStopReason(st *state.State, session string, now time.Time) string {
	if st.Guide == nil || st.Guide.Holder == nil || !st.Guide.Holder.Is(state.Party{Session: session}) {
		return ""
	}
	var ids []string
	for _, work := range st.GuideWork {
		if guideWorkBlocks(work, now) {
			ids = append(ids, fmt.Sprint(work.ID))
		}
	}
	if len(ids) == 0 {
		if st.Guide.WorkChecked == nil || !st.Guide.WorkChecked.Is(st.Guide.Holder.Party) {
			return "Before ending the guide turn, record every unfinished task, goal and restoration deadline with `beekeeper guide work add --due <time>`, then run `beekeeper guide work check`. Future work must be owned and timed in state, not left as a list in chat."
		}
		return ""
	}
	return "The guide still owns due work (" + strings.Join(ids, ", ") + "). Read `beekeeper guide work`, carry it out or file it, then close it with `beekeeper guide work done <id> --reason <result>`. Future actions must be recorded with their owner and deadline; a list in chat does not hand work over."
}
