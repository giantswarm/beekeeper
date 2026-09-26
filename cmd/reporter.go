package cmd

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/post"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// reporterParty is who the standby watch's reporter events are by.
var reporterParty = state.Party{Name: "beekeeper reporter"}

// reportAction is what the watch does about the reporter at a poll.
type reportAction int

const (
	reportNone reportAction = iota
	// reportStart starts the reporter of the current slot.
	reportStart
	// reportFinal starts the final report before the pause.
	reportFinal
	// reportSkip says the current slot skipped: the last one's still runs.
	reportSkip
	// reportPosted ends a reporter that posted.
	reportPosted
	// reportUnposted ends a reporter whose turn ended without a post.
	reportUnposted
	// reportTimeout stops a reporter that ran past the timeout.
	reportTimeout
)

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
			return reportFinal
		case pause.Paused():
		case r == nil || slot.After(r.Slot):
			return reportStart
		}
	case posted:
		return reportPosted
	case ended:
		return reportUnposted
	case now.Sub(r.Started) >= rc.Timeout.Duration:
		return reportTimeout
	case slot.After(r.Slot) && !r.Skipped.Equal(slot) && !pause.Paused():
		return reportSkip
	}
	return reportNone
}

// tendReporter runs the scheduled reporter, in the standby watch only: it
// starts one per slot, skips a slot while the last one runs, and takes a
// reporter that posted, ended or timed out off the roster and stops it.
func (w *watcher) tendReporter(ctx context.Context, sessions []*claude.Session) {
	rc := w.cfg.Reporter
	if !w.standby || !rc.Enabled() {
		return
	}
	st, err := w.store.Read()
	if err != nil {
		return // pending says it
	}
	r := st.Report
	var posted, ended bool
	if r.Running() {
		turnEnded := w.turnEnded
		if turnEnded == nil {
			turnEnded = unitEnded
		}
		posted = w.reportPosted(r.Session)
		ended = !posted && turnEnded(ctx, r.Unit)
	}
	switch action := reportDue(r, st.ReportPause, rc, w.now, posted, ended); action {
	case reportStart, reportFinal:
		err := w.startReport(ctx, sessions, action)
		w.check("reporter", err != nil, "REPORTER not started: %v", err)
	case reportSkip:
		w.skipReport()
	case reportPosted:
		w.endReport(ctx, "posted", "posted its report")
	case reportUnposted:
		w.endReport(ctx, "unposted", "its turn ended without a post")
	case reportTimeout:
		w.endReport(ctx, "timeout", fmt.Sprintf("no post within %s, stopped", dur(rc.Timeout.Duration)))
	}
}

// reportPosted reports whether the session's transcript holds the post.
func (w *watcher) reportPosted(id string) bool {
	m, _ := filepath.Glob(filepath.Join(w.cfg.Claude.ProjectsDir, "*", id+".jsonl"))
	if len(m) == 0 {
		return false
	}
	ok, _ := claude.Called(m[0], guard.PostTool) // unreadable: not yet
	return ok
}

// startReport starts the current slot's reporter: one headless turn in
// bypassPermissions in a transient user unit, registered on the roster busy
// with the brief's first line. It is not imported into the desktop: the
// command-line turn has the connectors, and the person's window stays put.
func (w *watcher) startReport(ctx context.Context, sessions []*claude.Session, action reportAction) error {
	rc := w.cfg.Reporter
	brief, err := readBrief(rc.Brief)
	if err != nil {
		return err
	}
	readZone := w.zone
	if readZone == nil {
		readZone = machine.Zone
	}
	zone, err := readZone()
	if err != nil {
		return err
	}
	slot := reportSlot(rc.Every.Duration, w.now)
	final := action == reportFinal
	// A scheduled report covers the interval before its slot; the final one
	// the time since the last report started, up to now.
	from, to := slot.Add(-rc.Every.Duration), slot
	if final {
		from, to = w.now.Add(-rc.Every.Duration), w.now
	}
	id := uuid.NewString()
	p := state.Party{Session: id, Name: "Status report " + to.In(zone).Format("15:04")}
	unit := "beekeeper-report-" + id[:8]
	live := func(x state.Party) bool {
		_, ok := claude.Live(sessions, x)
		return ok
	}
	started := false
	err = w.store.Update(func(st *state.State) ([]state.Event, error) {
		if reportDue(st.Report, st.ReportPause, rc, w.now, false, false) != action {
			return nil, nil // another watch started it
		}
		if final {
			if st.Report != nil {
				from = st.Report.Started
			}
			st.ReportPause.Final, st.ReportPause.Since = time.Time{}, w.now.UTC()
		}
		s := state.Start{Party: p, Mode: state.ModeBypass, Dir: rc.Dir, By: reporterParty, At: w.now.UTC()}
		reg, err := recordStart(st, s, briefTask(brief), live)
		if err != nil {
			return nil, err
		}
		st.Report = &state.Report{Party: p, Unit: unit, Slot: slot, Started: w.now.UTC()}
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
	run := w.runReport
	if run == nil {
		run = w.launchReport
	}
	if err := run(unit, id, p.Name, reportPrompt(rc, from, to, zone, final, brief)); err != nil {
		w.endReport(ctx, "failed", err.Error())
		return err
	}
	w.emitNow("reporter", "REPORTER %s started: session %s (journalctl --user -u %s)", p.Name, id, unit)
	return nil
}

// launchReport runs the reporter's turn in its transient user unit, with no
// ExecStopPost: nothing reopens it in the desktop. Its settings add the
// report check as a PreToolUse hook on the post, for this session only.
func (w *watcher) launchReport(unit, id, name, prompt string) error {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return launch(unit, w.cfg.Reporter.Dir, w.explicitConfig(), nil,
		agentArgv(bin, id, name, w.cfg.Reporter.Model, prompt, "--settings", reportSettings(self)))
}

// hookTypeCommand is a settings hook that runs a command.
const hookTypeCommand = "command"

// reportSettings are the reporter session's added settings: the PreToolUse
// hook that denies a post failing the report check.
func reportSettings(self string) string {
	hook := map[string]any{"type": hookTypeCommand, "command": guard.ShellQuote(self) + " hook reportcheck", "timeout": 10}
	raw, _ := json.Marshal(map[string]any{"hooks": map[string]any{
		"PreToolUse": []any{map[string]any{"matcher": ".*" + guard.PostTool, "hooks": []any{hook}}},
	}})
	return string(raw)
}

// reportPrompt is the reporter's first prompt: who the report is for, what
// it covers in the person's time zone (the machine's), and how the post must
// look, then the brief.
func reportPrompt(rc config.Reporter, from, to time.Time, zone *time.Location, final bool, brief string) string {
	from, to = from.In(zone), to.In(zone)
	span := from.Format("15:04") + "–" + to.Format("15:04 MST")
	last := ""
	if final {
		last = fmt.Sprintf("This is the last report before the %s reports pause until they are resumed: say so in one line at its end. ", dur(rc.Every.Duration))
	}
	return fmt.Sprintf("You are beekeeper's scheduled status reporter. %sReport %s to %s, "+
		"posted exactly once. Its first line gives the range %s; every time in the post is in %s (%s), never UTC. "+
		"The connector's message is standard Markdown, which it converts for Slack: every pull request or issue is a link "+
		"to it in its own repository, [<repo>#<n>](https://github.com/<owner>/<repo>/pull/<n>), never a bare #<n>, not even in a session's name; a beekeeper note is \"note <n>\": "+
		"check the text with `beekeeper reporter check` (it reads stdin) before posting; a post that fails the check is refused "+
		"with what to fix. Once the post has gone out, end your turn: beekeeper sees the post, "+
		"takes you off the roster and stops this session.\n\n%s",
		last, span, rc.Person, span, zone, to.Format("MST"), brief)
}

// skipReport records, once per slot, that the slot's reporter was not
// started because the last one still runs.
func (w *watcher) skipReport() {
	var line string
	_ = w.store.Update(func(st *state.State) ([]state.Event, error) {
		r := st.Report
		slot := reportSlot(w.cfg.Reporter.Every.Duration, w.now)
		if !r.Running() || r.Skipped.Equal(slot) {
			return nil, nil
		}
		r.Skipped = slot
		line = fmt.Sprintf("%s still runs (since %s): the %s report is skipped", r.Name, clock(w.now, r.Started), slot.Local().Format("15:04"))
		return []state.Event{event(reporterParty, "reporter.skip", "%s", line)}, nil
	})
	if line != "" {
		w.emitNow("reporter", "REPORTER %s", line)
	}
}

// endReport ends the running reporter with outcome: it takes it off the
// roster, then stops what still runs of its turn by PID.
func (w *watcher) endReport(ctx context.Context, outcome, detail string) {
	var r state.Report
	err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		if !st.Report.Running() {
			return nil, nil
		}
		st.Report.Ended, st.Report.Outcome = w.now.UTC(), outcome
		r = *st.Report
		st.Agents = slices.DeleteFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(r.Party) })
		return []state.Event{event(reporterParty, "reporter."+outcome, "%s: %s", r.Name, detail)}, nil
	})
	if err != nil || r.Session == "" {
		return
	}
	n, err := stopTurn(ctx, r.Session)
	line := fmt.Sprintf("REPORTER %s %s: %s, off the roster", r.Name, outcome, detail)
	switch {
	case err != nil:
		line += fmt.Sprintf("; stopping it: %v", err)
	case n > 0:
		line += fmt.Sprintf(", %d processes stopped", n)
	}
	w.emitNow("reporter", "%s", line)
}

// stopTurn stops the turn of session id and every process under it, by
// PID; 0 when none runs.
func stopTurn(ctx context.Context, id string) (int, error) {
	t, err := proc.Read()
	if err != nil {
		return 0, err
	}
	total := 0
	for _, pid := range turnPIDs(t, id) {
		n, err := endSession(ctx, &claude.Session{PID: pid})
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// turnPIDs are the claude processes that run session id: its first turn
// (--session-id) and any CLI resuming it.
func turnPIDs(t *proc.Table, id string) []int {
	var out []int
	for _, p := range t.ByPID {
		if startsSession(p, id) || p.Comm == claudeComm && resumes(p.Args, id) {
			out = append(out, p.PID)
		}
	}
	slices.Sort(out)
	return out
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
that does not name the machine's time zone (read live, as timedatectl sets
it), and a time in UTC.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			zone, err := machine.Zone()
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
