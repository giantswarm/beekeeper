package cmd

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/post"
	"github.com/giantswarm/beekeeper/internal/state"
)

// reporterParty is who the standby watch's reporter events are by.
var reporterParty = state.Party{Name: "beekeeper reporter"}

// reportSlot is the start of the interval slot now is in: a multiple of
// every since the zero time, on the hour for 1h.
func reportSlot(every time.Duration, now time.Time) time.Time { return now.UTC().Truncate(every) }

// reportDue decides what to do about the reporter r at now under pause:
// posted and ended say what its running session did.
func reportDue(r *state.Report, pause *state.ReportPause, rc config.Reporter, now time.Time, posted, ended bool) reportAction {
	slot := reportSlot(rc.Every.Duration, now)
	switch {
	case !r.Running():
		switch {
		case pause.FinalDue(now):
			return ReportFinal
		case pause.Paused():
		case r == nil || slot.After(r.Slot):
			return ReportStart
		}
	case posted:
		return ReportPosted
	case ended:
		return ReportUnposted
	case now.Sub(r.Started) >= rc.Timeout.Duration:
		return ReportTimeout
	case slot.After(r.Slot) && !r.Skipped.Equal(slot) && !pause.Paused():
		return ReportSkip
	}
	return ReportNone
}

// watchStartReport starts the current slot's reporter: one headless turn in
// bypassPermissions in a transient user unit, registered on the roster busy
// with the brief's first line. It is not imported into the desktop: the
// command-line turn has the connectors, and the person's window stays put.
func watchStartReport(w *Watcher, ctx context.Context, sessions []*claude.Session, action reportAction) error {
	rc := watchApp(w.Host).cfg.Reporter
	brief, err := readBrief(rc.Brief)
	if err != nil {
		return err
	}
	zone, err := reportZone(rc.TZ, watchApp(w.Host).zone)
	if err != nil {
		return err
	}
	slot := reportSlot(rc.Every.Duration, watchApp(w.Host).now)
	final := action == ReportFinal
	// A scheduled report covers the interval before its slot; the final one
	// the time since the last report started, up to now.
	from, to := slot.Add(-rc.Every.Duration), slot
	if final {
		from, to = watchApp(w.Host).now.Add(-rc.Every.Duration), watchApp(w.Host).now
	}
	id := uuid.NewString()
	p := state.Party{Session: id, Name: "Status report " + to.In(zone).Format("15:04")}
	unit := "beekeeper-report-" + id[:8]
	live := func(x state.Party) bool {
		_, ok := claude.Live(sessions, x)
		return ok
	}
	started := false
	err = watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		if reportDue(st.Report, st.ReportPause, rc, watchApp(w.Host).now, false, false) != action {
			return nil, nil // another watch started it
		}
		if st.Stop != nil {
			return nil, nil // the report starts after beekeeper resume
		}
		if final {
			if st.Report != nil {
				from = st.Report.Started
			}
			st.ReportPause.Final, st.ReportPause.Since = time.Time{}, watchApp(w.Host).now.UTC()
		}
		s := state.Start{Party: p, Mode: state.ModeBypass, Dir: rc.Dir, By: reporterParty, At: watchApp(w.Host).now.UTC()}
		reg, err := watchApp(w.Host).recordStart(st, s, briefTask(brief), "", live)
		if err != nil {
			return nil, err
		}
		st.Report = &state.Report{Party: p, Unit: unit, Slot: slot, Started: watchApp(w.Host).now.UTC()}
		started = true
		evs := []state.Event{event(reporterParty, "reporter.start", "%s: session %s for %s, busy with %q", p.Name, id, rc.Person, reg.task)}
		if final {
			evs = append(evs, event(reporterParty, "reporter.pause", "after the final report %s, until beekeeper reporter resume", p.Name))
		}
		return evs, nil
	})
	if err != nil || !started {
		return err
	}
	run := w.RunReport
	if run == nil {
		run = w.LaunchReport
	}
	if err := run(unit, id, p.Name, reportPrompt(rc, from, to, zone, final, brief)); err != nil {
		w.EndReport(ctx, "failed", err.Error())
		return err
	}
	w.EmitNow("reporter", "REPORTER %s started: session %s (journalctl --user -u %s)", p.Name, id, unit)
	return nil
}

// hookTypeCommand is a settings hook that runs a command; hooksKey holds a
// settings document's hooks and a matcher's.
const (
	hookTypeCommand = "command"
	hooksKey        = "hooks"
)

// reportPrompt is the reporter's first prompt: who the report is for, the
// beekeeper report command that renders it for the interval, in the
// report's time zone, and how the post must look, then the brief.
func reportPrompt(rc config.Reporter, from, to time.Time, zone *time.Location, final bool, brief string) string {
	span := from.In(zone).Format("15:04") + "–" + to.In(zone).Format("15:04 MST")
	last := ""
	if final {
		last = fmt.Sprintf("This is the last report before the %s reports pause until they are resumed: say so in one line at its end. ", dur(rc.Every.Duration))
	}
	render := fmt.Sprintf("beekeeper report --since %s --until %s", from.In(zone).Format(time.RFC3339), to.In(zone).Format(time.RFC3339))
	return fmt.Sprintf("You are beekeeper's scheduled status reporter. %sReport %s to %s, posted exactly once. "+
		"Run `%s` once: it renders the whole report from beekeeper's own state, its first line the range %s, "+
		"every time in %s (%s), never UTC, every pull request or issue a link to its own repository, and it passes "+
		"`beekeeper reporter check`. Post its output unchanged; you may add at most three sentences of your own judgement "+
		"at its end, or none, and run nothing else. The connector's message is standard Markdown, which it converts for Slack: "+
		"a pull request or issue you name is [<repo>#<n>](https://github.com/<owner>/<repo>/pull/<n>), never a bare #<n>; "+
		"a beekeeper note is \"note <n>\". A post that fails the check is refused with what to fix. Once the post has gone out, "+
		"end your turn: beekeeper sees the post, takes you off the roster and stops this session.\n\n%s",
		last, span, rc.Person, render, span, zone, to.In(zone).Format("MST"), brief)
}

func (a *app) reporterCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "reporter",
		Short: "The scheduled status reporter: its schedule, the current or last run",
		Long: `The standby watch (watch --standby, beekeeper-notify.service) starts a
one-off reporter session once per reporter.every, on the interval's
multiples (on the hour for 1h): one headless turn in bypassPermissions
with reporter.brief, registered on the roster as "Status report HH:MM".
The session posts one report to reporter.person with its connector and
ends. beekeeper sees the post in its transcript (a successful
slack_send_message call), takes it off the roster and stops what still
runs of it; a turn that ended without a post, or that has not posted
within reporter.timeout, is ended the same way and said in the event log
and the watch. While a reporter runs, the next slot is skipped, once.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			rc := a.cfg.Reporter
			if !rc.Enabled() {
				_, err := fmt.Fprintln(a.out, "no reporter is scheduled: set reporter.every and reporter.brief")
				return err
			}
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			next := "next " + clock(a.now, reportSlot(rc.Every.Duration, a.now).Add(rc.Every.Duration))
			if r := st.Report; r == nil || reportSlot(rc.Every.Duration, a.now).After(r.Slot) && !r.Running() {
				next = "next " + clock(a.now, a.now)
			}
			switch pause := st.ReportPause; {
			case pause.Paused():
				next = fmt.Sprintf("paused since %s by %s: beekeeper reporter resume", clock(a.now, pause.Since), pause.By.Name)
			case pause != nil:
				next += fmt.Sprintf("; the final report at %s, then paused (set by %s)", clock(a.now, pause.Final), pause.By.Name)
			}
			model := cmp.Or(rc.Model, "Claude Code's default")
			_, err = fmt.Fprintf(a.out, "every %s for %s, brief %s, model %s, timeout %s; %s\n",
				dur(rc.Every.Duration), rc.Person, rc.Brief, model, dur(rc.Timeout.Duration), next)
			if err != nil || st.Report == nil {
				return err
			}
			r := st.Report
			status := "runs"
			if !r.Running() {
				status = r.Outcome + " " + clock(a.now, r.Ended)
			}
			_, err = fmt.Fprintf(a.out, "last: %s, session %s, started %s, %s\n", r.Name, r.Session, clock(a.now, r.Started), status)
			return err
		},
	}
	c.AddCommand(a.reporterFinalCmd(), a.reporterPauseCmd(), a.reporterResumeCmd(), &cobra.Command{
		Use:   "check",
		Short: "Check a report on stdin: every pull request and issue linked to its repository, the times in the machine's zone",
		Long: `check reads a report from stdin and prints what is wrong with it, one
line each, exiting 3; a report that passes prints "ok". It is the check
the reporter's post passes: a reporter session's slack_send_message is
refused by its PreToolUse hook (beekeeper hook reportcheck) until the
message passes. The Slack connector takes standard Markdown and converts
it for Slack, so links are [label](url). It refuses Slack's own <url|label>
syntax, every #<n> or [owner/]repo#<n> outside a link, session names
included (a beekeeper note or timer is "note <n>", without the #), a
GitHub pull request or issue
link whose label does not name its repository and number, a first line
that does not name the report's time zone (reporter.tz, else the machine's,
read live as timedatectl sets it), and a time in UTC.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			zone, err := reportZone(a.cfg.Reporter.TZ, nil)
			if err != nil {
				return err
			}
			if p := post.Report(string(raw), zone, time.Now()); len(p) > 0 {
				return refused("%s", strings.Join(p, "\n"))
			}
			_, err = fmt.Fprintln(a.out, "ok")
			return err
		},
	})
	return c
}

func (a *app) reporterFinalCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "final <time>",
		Short: "One final report at a time (06:45) or after a duration (45m), then pause the schedule",
		Long: `final schedules one last report at <time>, outside the interval's slots,
covering the time since the last report started; the hourly reports run
until then. With its start the schedule pauses: no report starts until
beekeeper reporter resume. A final set again replaces the last one.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			at, err := untilTime(a.now, args[0])
			if err != nil {
				return err
			}
			if at.IsZero() {
				return usageErr("final needs a time")
			}
			return a.setReportPause(&state.ReportPause{Final: at.UTC()}, "reporter.final",
				"the final report at %s, then paused until beekeeper reporter resume", clock(a.now, at))
		},
	}
}

func (a *app) reporterPauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause",
		Short: "Pause the scheduled reports now, until reporter resume",
		Long:  `pause starts no scheduled report from now on; a running reporter still posts and is ended. beekeeper reporter resume ends the pause.`,
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return a.setReportPause(&state.ReportPause{Since: a.now.UTC()}, "reporter.pause", "paused until beekeeper reporter resume")
		},
	}
}

func (a *app) reporterResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Resume the scheduled reports: the current slot's report starts at once",
		Long: `resume ends a pause or a pending final: the standby watch starts the current
slot's report at its next poll, then one per interval.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return a.setReportPause(nil, "reporter.resume", "the scheduled reports run again")
		},
	}
}

// setReportPause records pause (nil: none) for the caller and says so.
func (a *app) setReportPause(pause *state.ReportPause, verb, format string, args ...any) error {
	if !a.cfg.Reporter.Enabled() {
		return refused("no reporter is scheduled: set reporter.every and reporter.brief")
	}
	me, err := a.caller()
	if err != nil {
		return err
	}
	line := fmt.Sprintf(format, args...)
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		if pause != nil {
			pause.By = me
		}
		st.ReportPause = pause
		return []state.Event{event(me, verb, "%s", line)}, nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.out, "reporter: "+line)
	return err
}
