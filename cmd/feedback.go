package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"

	"github.com/giantswarm/beekeeper/internal/central"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/feedback"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
)

// feedbackParty is who the feedback watch's events and messages are by.
var feedbackParty = state.Party{Name: "beekeeper feedback"}

// feedbackEmoji is the reaction a delivered reply gets in Slack.
const feedbackEmoji = "eyes"

// recordReportThread keeps the Slack thread of the report r posted, which
// the feedback watch reads; a post whose result names no thread is said.
func (w *watcher) recordReportThread(r state.Report) {
	if !w.cfg.Feedback.Enabled() {
		return
	}
	m, _ := filepath.Glob(filepath.Join(w.cfg.Claude.ProjectsDir, "*", r.Session+".jsonl"))
	if len(m) == 0 {
		return
	}
	text, ok, err := claude.CallResult(m[0], guard.PostTool)
	if err != nil || !ok {
		return
	}
	p, err := feedback.ParsePost(text)
	if err != nil {
		w.emitNow("feedback", "FEEDBACK %s: no thread to read: %v", r.Name, err)
		return
	}
	_ = w.store.Update(func(st *state.State) ([]state.Event, error) {
		if slices.ContainsFunc(st.ReportThreads, func(t state.ReportThread) bool { return t.Channel == p.Channel && t.TS == p.TS }) {
			return nil, nil
		}
		st.ReportThreads = append(st.ReportThreads, state.ReportThread{Report: r.Name, Channel: p.Channel, TS: p.TS, Posted: w.now.UTC()})
		return nil, nil
	})
}

// tendFeedback reads the report threads once per feedback.every, in the
// standby watch only, beside the poll: a slow muster never holds it up.
func (w *watcher) tendFeedback(ctx context.Context) {
	fc := w.cfg.Feedback
	if !w.standby || !fc.Enabled() || w.now.Before(w.feedbackNext) || !w.feedbackBusy.CompareAndSwap(false, true) {
		return
	}
	w.feedbackNext = w.now.Add(fc.Every.Duration)
	quiet := *w.app
	quiet.out = io.Discard
	go func() {
		defer w.feedbackBusy.Store(false)
		w.readFeedback(ctx, &quiet)
	}()
}

// readFeedback delivers the replies of each report's author in its thread
// that came since the last delivered one, oldest first: to the agent a
// reply names, else to the supervisor. A delivered reply gets the
// feedbackEmoji reaction; one not delivered is read again next time.
// Threads older than feedback.window are dropped. It runs beside the poll on
// a, the watch's app as of its start with its output discarded.
func (w *watcher) readFeedback(ctx context.Context, a *app) {
	fc := a.cfg.Feedback
	st, err := a.store.Read()
	if err != nil {
		return
	}
	since := a.now.Add(-fc.Window.Duration)
	c := central.New(config.Central{Context: fc.Context, Server: fc.Server, Muster: a.cfg.Central.Muster, Timeout: a.cfg.Central.Timeout}, w.musterRun)
	var unreadable error
	for _, th := range st.ReportThreads {
		if th.Posted.Before(since) {
			continue
		}
		text, err := c.Call(ctx, "read_thread", map[string]any{"channel_id": th.Channel, "message_ts": th.TS}, nil)
		if err != nil {
			unreadable = err
			break
		}
		t, err := feedback.ParseThread(text)
		if err != nil {
			unreadable = err
			break
		}
		seen := th.Seen
		for _, r := range t.Replies {
			if r.User != t.Parent.User || !feedback.Later(r.TS, seen) {
				continue
			}
			if !w.deliverFeedback(ctx, a, c, th, r) {
				break
			}
			seen = r.TS
		}
		if seen != th.Seen {
			markFeedback(a.store, th, seen)
		}
	}
	w.check("feedback", unreadable != nil, "FEEDBACK UNREADABLE %s: %s", fc.Context, unreadableReason(unreadable))
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.ReportThreads = slices.DeleteFunc(st.ReportThreads, func(t state.ReportThread) bool { return t.Posted.Before(since) })
		return nil, nil
	})
}

// deliverFeedback delivers reply r on thread th and reports whether it was.
func (w *watcher) deliverFeedback(ctx context.Context, a *app, c *central.Client, th state.ReportThread, r feedback.Message) bool {
	st, err := a.store.Read()
	if err != nil {
		return false
	}
	names := make([]string, 0, len(st.Agents))
	for _, ag := range st.Agents {
		names = append(names, ag.Name)
	}
	to, msg := feedback.Route(r.Text, names)
	label := to
	if to == "" {
		if st.Supervisor == nil {
			w.emitNow("feedback", "FEEDBACK on %s waits: no supervisor to deliver it to", th.Report)
			return false
		}
		to, label = st.Supervisor.Session, "the supervisor"
	}
	person := a.cfg.Reporter.Person
	body := fmt.Sprintf("Feedback from %s in Slack, a reply to the status report %s: %s", person, th.Report, msg)
	deliver := w.deliver
	if deliver == nil {
		deliver = func(ctx context.Context, q, msg string) error { return a.wakeAgent(ctx, feedbackParty, q, msg, "") }
	}
	if err := deliver(ctx, to, body); err != nil {
		w.emitNow("feedback", "FEEDBACK on %s not delivered to %s, read again next time: %v", th.Report, label, err)
		return false
	}
	_ = a.store.Log(event(feedbackParty, "feedback.delivered", "%s: a reply of %s to %s", label, person, th.Report))
	line := fmt.Sprintf("FEEDBACK to %s, a reply to %s: %s", label, th.Report, firstLine(msg))
	if _, err := c.Call(ctx, "add_reaction", map[string]any{"channel_id": th.Channel, "message_ts": r.TS, "emoji": feedbackEmoji}, nil); err != nil {
		line += fmt.Sprintf(" (no :%s: reaction: %s)", feedbackEmoji, unreadableReason(err))
	}
	w.emitNow("feedback", "%s", line)
	return true
}

// markFeedback records seen as the last reply delivered on th.
func markFeedback(store state.Store, th state.ReportThread, seen string) {
	_ = store.Update(func(st *state.State) ([]state.Event, error) {
		for i, t := range st.ReportThreads {
			if t.Channel == th.Channel && t.TS == th.TS {
				st.ReportThreads[i].Seen = seen
			}
		}
		return nil, nil
	})
}

// unreadableReason is a muster call's error without the central instance's
// wording: the reason alone.
func unreadableReason(err error) string {
	var u *central.Unreachable
	switch {
	case err == nil:
		return ""
	case errors.As(err, &u):
		return u.Reason
	}
	return err.Error()
}
