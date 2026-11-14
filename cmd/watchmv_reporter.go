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

// tendReporter runs the scheduled reporter, in the standby watch only: it
// starts one per slot, skips a slot while the last one runs, and takes a
// reporter that posted, ended or timed out off the roster and stops it.
func (w *watcher) tendReporter(ctx context.Context, sessions []*claude.Session) {
	rc := w.a.cfg.Reporter
	if !w.standby || !rc.Enabled() {
		return
	}
	st, err := w.a.store.Peek()
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
	switch action := reportDue(r, st.ReportPause, rc, w.a.now, posted, ended); action {
	case reportStart, reportFinal:
		err := w.startReport(ctx, sessions, action)
		w.check("reporter", err != nil, "REPORTER not started: %v", err)
	case reportSkip:
		w.skipReport()
	case reportPosted:
		w.recordReportThread(*r)
		w.endReport(ctx, "posted", "posted its report")
	case reportUnposted:
		w.endReport(ctx, "unposted", "its turn ended without a post")
	case reportTimeout:
		w.endReport(ctx, "timeout", fmt.Sprintf("no post within %s, stopped", dur(rc.Timeout.Duration)))
	}
}

// reportPosted reports whether the session's transcript holds the post.
func (w *watcher) reportPosted(id string) bool {
	path := transcriptOf(w.a.cfg, id)
	if path == "" {
		return false
	}
	ok, _ := claude.Called(path, guard.PostTool) // unreadable: not yet
	return ok
}

// startReport runs in the command, which holds what it needs (watchStartReport).
func (w *watcher) startReport(ctx context.Context, sessions []*claude.Session, action reportAction) error {
	return watchStartReport(w, ctx, sessions, action)
}

// launchReport runs the reporter's turn in its transient user unit, with no
// ExecStopPost: nothing reopens it in the desktop. Its settings add the
// report check as a PreToolUse hook on the post, for this session only.
func (w *watcher) launchReport(unit, id, name, prompt string) error {
	if err := w.a.requireClaudeLogin(); err != nil {
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
	return launch(unit, w.a.cfg.Reporter.Dir, w.a.explicitConfig(), nil,
		agentArgv(bin, id, name, w.a.cfg.Reporter.Model, prompt, "--settings", reportSettings(self)))
}

// reportSettings are the reporter session's added settings: the PreToolUse
// hook that denies a post failing the report check.
func reportSettings(self string) string {
	hook := map[string]any{"type": hookTypeCommand, "command": guard.ShellQuote(self) + " hook reportcheck", "timeout": 10}
	raw, _ := json.Marshal(map[string]any{hooksKey: map[string]any{
		"PreToolUse": []any{map[string]any{"matcher": ".*" + guard.PostTool, hooksKey: []any{hook}}},
	}})
	return string(raw)
}

// skipReport records, once per slot, that the slot's reporter was not
// started because the last one still runs.
func (w *watcher) skipReport() {
	var line string
	_ = w.a.store.Update(func(st *state.State) ([]state.Event, error) {
		r := st.Report
		slot := reportSlot(w.a.cfg.Reporter.Every.Duration, w.a.now)
		if !r.Running() || r.Skipped.Equal(slot) {
			return nil, nil
		}
		r.Skipped = slot
		line = fmt.Sprintf("%s still runs (since %s): the %s report is skipped", r.Name, clock(w.a.now, r.Started), slot.Local().Format("15:04"))
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
	err := w.a.store.Update(func(st *state.State) ([]state.Event, error) {
		if !st.Report.Running() {
			return nil, nil
		}
		st.Report.Ended, st.Report.Outcome = w.a.now.UTC(), outcome
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
	t, err := plat.Machine.Processes()
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
