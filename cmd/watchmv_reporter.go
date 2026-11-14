package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// reportAction is what the watch does about the reporter at a poll.
type reportAction int

const (
	ReportNone reportAction = iota
	// ReportStart starts the reporter of the current slot.
	ReportStart
	// ReportFinal starts the final report before the pause.
	ReportFinal
	// ReportSkip says the current slot skipped: the last one's still runs.
	ReportSkip
	// ReportPosted ends a reporter that posted.
	ReportPosted
	// ReportUnposted ends a reporter whose turn ended without a post.
	ReportUnposted
	// ReportTimeout stops a reporter that ran past the timeout.
	ReportTimeout
)

// TendReporter runs the scheduled reporter, in the standby watch only: it
// starts one per slot, skips a slot while the last one runs, and takes a
// reporter that posted, ended or timed out off the roster and stops it.
func (w *Watcher) TendReporter(ctx context.Context, sessions []*claude.Session) {
	rc := w.Host.Cfg().Reporter
	if !w.Standby || !rc.Enabled() {
		return
	}
	st, err := w.Host.Store().Peek()
	if err != nil {
		return // pending says it
	}
	r := st.Report
	var posted, ended bool
	if r.Running() {
		turnEnded := w.TurnEnded
		if turnEnded == nil {
			turnEnded = Cmd.UnitEnded
		}
		posted = w.reportPosted(r.Session)
		ended = !posted && turnEnded(ctx, r.Unit)
	}
	switch action := Cmd.ReportDue(r, st.ReportPause, rc, w.Host.Now(), posted, ended); action {
	case ReportStart, ReportFinal:
		err := w.startReport(ctx, sessions, action)
		w.Check("reporter", err != nil, "REPORTER not started: %v", err)
	case ReportSkip:
		w.skipReport()
	case ReportPosted:
		w.recordReportThread(*r)
		w.EndReport(ctx, "posted", "posted its report")
	case ReportUnposted:
		w.EndReport(ctx, "unposted", "its turn ended without a post")
	case ReportTimeout:
		w.EndReport(ctx, "timeout", fmt.Sprintf("no post within %s, stopped", Cmd.Dur(rc.Timeout.Duration)))
	}
}

// reportPosted reports whether the session's transcript holds the post.
func (w *Watcher) reportPosted(id string) bool {
	path := Cmd.TranscriptOf(w.Host.Cfg(), id)
	if path == "" {
		return false
	}
	ok, _ := claude.Called(path, guard.PostTool) // unreadable: not yet
	return ok
}

// startReport runs in the command, which holds what it needs (watchStartReport).
func (w *Watcher) startReport(ctx context.Context, sessions []*claude.Session, action reportAction) error {
	return Cmd.WatchStartReport(w, ctx, sessions, action)
}

// LaunchReport runs the reporter's turn in its transient user unit, with no
// ExecStopPost: nothing reopens it in the desktop. Its settings add the
// report check as a PreToolUse hook on the post, for this session only.
func (w *Watcher) LaunchReport(unit, id, name, prompt string) error {
	if err := w.Host.RequireClaudeLogin(); err != nil {
		return err
	}

	bin, err := proc.LookPath("claude")
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return Cmd.Launch(unit, w.Host.Cfg().Reporter.Dir, w.Host.ExplicitConfig(), nil,
		Cmd.AgentArgv(bin, id, name, w.Host.Cfg().Reporter.Model, prompt, "--settings", ReportSettings(self)))
}

// ReportSettings are the reporter session's added settings: the PreToolUse
// hook that denies a post failing the report check.
func ReportSettings(self string) string {
	hook := map[string]any{"type": hookTypeCommand, "command": guard.ShellQuote(self) + " hook reportcheck", "timeout": 10}
	raw, _ := json.Marshal(map[string]any{hooksKey: map[string]any{
		"PreToolUse": []any{map[string]any{"matcher": ".*" + guard.PostTool, hooksKey: []any{hook}}},
	}})
	return string(raw)
}

// skipReport records, once per slot, that the slot's reporter was not
// started because the last one still runs.
func (w *Watcher) skipReport() {
	var line string
	_ = w.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		r := st.Report
		slot := Cmd.ReportSlot(w.Host.Cfg().Reporter.Every.Duration, w.Host.Now())
		if !r.Running() || r.Skipped.Equal(slot) {
			return nil, nil
		}
		r.Skipped = slot
		line = fmt.Sprintf("%s still runs (since %s): the %s report is skipped", r.Name, Cmd.Clock(w.Host.Now(), r.Started), slot.Local().Format("15:04"))
		return []state.Event{Cmd.Event(Cmd.ReporterParty(), "reporter.skip", "%s", line)}, nil
	})
	if line != "" {
		w.EmitNow("reporter", "REPORTER %s", line)
	}
}

// EndReport ends the running reporter with outcome: it takes it off the
// roster, then stops what still runs of its turn by PID.
func (w *Watcher) EndReport(ctx context.Context, outcome, detail string) {
	var r state.Report
	err := w.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		if !st.Report.Running() {
			return nil, nil
		}
		st.Report.Ended, st.Report.Outcome = w.Host.Now().UTC(), outcome
		r = *st.Report
		st.Agents = slices.DeleteFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(r.Party) })
		return []state.Event{Cmd.Event(Cmd.ReporterParty(), "reporter."+outcome, "%s: %s", r.Name, detail)}, nil
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
	w.EmitNow("reporter", "%s", line)
}

// stopTurn stops the turn of session id and every process under it, by
// PID; 0 when none runs.
func stopTurn(ctx context.Context, id string) (int, error) {
	t, err := Cmd.Plat().Machine.Processes()
	if err != nil {
		return 0, err
	}
	total := 0
	for _, pid := range TurnPIDs(t, id) {
		n, err := Cmd.EndSession(ctx, &claude.Session{PID: pid})
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// TurnPIDs are the claude processes that run session id: its first turn
// (--session-id) and any CLI resuming it.
func TurnPIDs(t *proc.Table, id string) []int {
	var out []int
	for _, p := range t.ByPID {
		if Cmd.StartsSession(p, id) || p.Comm == claudeComm && Cmd.Resumes(p.Args, id) {
			out = append(out, p.PID)
		}
	}
	slices.Sort(out)
	return out
}
