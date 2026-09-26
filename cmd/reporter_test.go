package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

var reportNow = time.Date(2026, 9, 26, 23, 0, 30, 0, time.UTC)

func hourly() config.Reporter {
	return config.Reporter{Every: config.Duration{Duration: time.Hour}, Brief: "b.md", Timeout: config.Duration{Duration: 20 * time.Minute}}
}

func TestReportDue(t *testing.T) {
	slot := reportSlot(time.Hour, reportNow)
	last := slot.Add(-time.Hour)
	for _, c := range []struct {
		name          string
		r             *state.Report
		pause         *state.ReportPause
		now           time.Time
		posted, ended bool
		want          reportAction
	}{
		{"never ran", nil, nil, reportNow, false, false, reportStart},
		{"the last slot's ended", &state.Report{Slot: last, Started: last, Ended: last.Add(time.Minute)}, nil, reportNow, false, false, reportStart},
		{"this slot's ended", &state.Report{Slot: slot, Started: slot, Ended: slot.Add(time.Minute)}, nil, reportNow.Add(30 * time.Minute), false, false, reportNone},
		{"runs", &state.Report{Slot: slot, Started: slot}, nil, reportNow.Add(5 * time.Minute), false, false, reportNone},
		{"posted", &state.Report{Slot: slot, Started: slot}, nil, reportNow, true, true, reportPosted},
		{"ended without a post", &state.Report{Slot: slot, Started: slot}, nil, reportNow, false, true, reportUnposted},
		{"past the timeout", &state.Report{Slot: slot, Started: slot}, nil, slot.Add(20 * time.Minute), false, false, reportTimeout},
		{"the next slot while it runs", &state.Report{Slot: last, Started: slot.Add(-5 * time.Minute)}, nil, reportNow, false, false, reportSkip},
		{"the next slot, skipped already", &state.Report{Slot: last, Started: slot.Add(-5 * time.Minute), Skipped: slot}, nil, reportNow, false, false, reportNone},
		{"paused", &state.Report{Slot: last, Started: last, Ended: last.Add(time.Minute)}, &state.ReportPause{Since: last}, reportNow, false, false, reportNone},
		{"paused while one runs: no skip", &state.Report{Slot: last, Started: slot.Add(-5 * time.Minute)}, &state.ReportPause{Since: slot}, reportNow, false, false, reportNone},
		{"a final not yet due", &state.Report{Slot: slot, Started: slot, Ended: slot.Add(time.Minute)}, &state.ReportPause{Final: slot.Add(45 * time.Minute)}, slot.Add(30 * time.Minute), false, false, reportNone},
		{"a final not yet due, the next slot", &state.Report{Slot: last, Started: last, Ended: last.Add(time.Minute)}, &state.ReportPause{Final: slot.Add(45 * time.Minute)}, reportNow, false, false, reportStart},
		{"the final due", &state.Report{Slot: slot, Started: slot, Ended: slot.Add(time.Minute)}, &state.ReportPause{Final: slot.Add(45 * time.Minute)}, slot.Add(45 * time.Minute), false, false, reportFinal},
		{"the final due while one runs", &state.Report{Slot: slot, Started: slot}, &state.ReportPause{Final: slot.Add(5 * time.Minute)}, slot.Add(6 * time.Minute), false, false, reportNone},
	} {
		if got := reportDue(c.r, c.pause, hourly(), c.now, c.posted, c.ended); got != c.want {
			t.Errorf("%s: reportDue = %d, want %d", c.name, got, c.want)
		}
	}
}

// reportingWatch is a standby watch with an hourly reporter on the state in
// dir; launched collects the turns it starts.
func reportingWatch(t *testing.T, dir string) (*watcher, *[]string, *bytes.Buffer) {
	t.Helper()
	w, _, out := notifyingWatch(t, dir, true)
	w.notifier = nil
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("# Hourly status\n\nPost it."), 0o600); err != nil {
		t.Fatal(err)
	}
	w.cfg.Reporter = hourly()
	w.cfg.Reporter.Brief, w.cfg.Reporter.Dir, w.cfg.Reporter.Person = brief, dir, "Ada"
	w.cfg.Claude.ProjectsDir = filepath.Join(dir, "projects")
	w.now = reportNow
	var launched []string
	w.turnEnded = func(context.Context, string) bool { return false }
	w.zone = func() (*time.Location, error) { return time.LoadLocation("Europe/Athens") }
	w.runReport = func(unit, id, name, prompt string) error {
		if !strings.HasPrefix(unit, "beekeeper-report-"+id[:8]) || !strings.Contains(prompt, "to Ada") || !strings.Contains(prompt, "–"+strings.TrimPrefix(name, "Status report ")+" EEST; every time in the post is in Europe/Athens") || !strings.Contains(prompt, "beekeeper reporter check") || !strings.HasSuffix(prompt, "Post it.") {
			t.Errorf("launch %s %s %q", unit, name, prompt)
		}
		launched = append(launched, name)
		return nil
	}
	return w, &launched, out
}

func TestReporterStartsOncePerSlotAcrossWatches(t *testing.T) {
	dir := t.TempDir()
	a, la, _ := reportingWatch(t, dir)
	b, lb, _ := reportingWatch(t, dir)
	ctx := context.Background()
	a.tendReporter(ctx, nil)
	b.tendReporter(ctx, nil)
	if names := append(*la, *lb...); len(names) != 1 || names[0] != "Status report 02:00" {
		t.Fatalf("started %v and %v, want one reporter, Status report 02:00", *la, *lb)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	r := st.Report
	if !r.Running() || !r.Slot.Equal(reportSlot(time.Hour, reportNow)) || len(st.Agents) != 1 || !st.Agents[0].Is(r.Party) ||
		st.Agents[0].Task != "Hourly status" {
		t.Errorf("report %+v, agents %+v", r, st.Agents)
	}
	if _, ok := st.BypassStart(r.Session); !ok {
		t.Error("the start is not recorded")
	}
}

func TestReporterSkipsTheNextSlotWhileItRuns(t *testing.T) {
	dir := t.TempDir()
	w, launched, out := reportingWatch(t, dir)
	ctx := context.Background()
	w.tendReporter(ctx, nil)
	w.cfg.Reporter.Timeout.Duration = 2 * time.Hour
	w.now = reportNow.Add(time.Hour)
	w.tendReporter(ctx, nil)
	w.tendReporter(ctx, nil)
	if len(*launched) != 1 || strings.Count(out.String(), "is skipped") != 1 {
		t.Errorf("launched %v; out:\n%s", *launched, out)
	}
	if evs := verbs(t, w, "reporter.skip"); evs != 1 {
		t.Errorf("%d reporter.skip events, want 1", evs)
	}
}

func TestReporterEndsOnThePost(t *testing.T) {
	dir := t.TempDir()
	w, _, out := reportingWatch(t, dir)
	ctx := context.Background()
	w.tendReporter(ctx, nil)
	st, _ := w.store.Read()
	id := st.Report.Session
	proj := filepath.Join(w.cfg.Claude.ProjectsDir, "p")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	post := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"mcp__claude_ai_Slack__slack_send_message","input":{}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"sent"}]}}
`
	if err := os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte(post), 0o600); err != nil {
		t.Fatal(err)
	}
	w.now = reportNow.Add(3 * time.Minute)
	w.tendReporter(ctx, nil)
	st, _ = w.store.Read()
	if st.Report.Running() || st.Report.Outcome != "posted" || len(st.Agents) != 0 {
		t.Errorf("report %+v, agents %+v", st.Report, st.Agents)
	}
	if !strings.Contains(out.String(), "posted: posted its report, off the roster") || verbs(t, w, "reporter.posted") != 1 {
		t.Errorf("out:\n%s", out)
	}
	w.now = reportNow.Add(time.Hour)
	w.tendReporter(ctx, nil)
	if st, _ = w.store.Read(); !st.Report.Running() || st.Report.Session == id {
		t.Errorf("the next slot's report %+v", st.Report)
	}
}

func TestReporterTimesOut(t *testing.T) {
	dir := t.TempDir()
	w, _, out := reportingWatch(t, dir)
	ctx := context.Background()
	w.tendReporter(ctx, nil)
	w.now = reportNow.Add(20 * time.Minute)
	w.tendReporter(ctx, nil)
	st, _ := w.store.Read()
	if st.Report.Running() || st.Report.Outcome != "timeout" || len(st.Agents) != 0 {
		t.Errorf("report %+v, agents %+v", st.Report, st.Agents)
	}
	if !strings.Contains(out.String(), "timeout: no post within 20m, stopped") || verbs(t, w, "reporter.timeout") != 1 {
		t.Errorf("out:\n%s", out)
	}
}

func TestReporterOnlyInTheStandbyWatch(t *testing.T) {
	w, launched, _ := reportingWatch(t, t.TempDir())
	w.standby = false
	w.tendReporter(context.Background(), nil)
	if len(*launched) != 0 {
		t.Errorf("a supervisor's watch started %v", *launched)
	}
}

func TestReporterNotStartedSaysWhyOnce(t *testing.T) {
	w, _, out := reportingWatch(t, t.TempDir())
	w.cfg.Reporter.Brief = filepath.Join(t.TempDir(), "missing.md")
	for range 2 {
		w.tendReporter(context.Background(), nil)
	}
	if strings.Count(out.String(), "REPORTER not started") != 1 {
		t.Errorf("out:\n%s", out)
	}
}

func TestTurnPIDs(t *testing.T) {
	tb := &proc.Table{ByPID: map[int]*proc.Process{
		10: {PID: 10, Comm: claudeComm, Args: []string{claudeComm, "-p", sessionIDFlag, "s1", "--", "brief"}},
		11: {PID: 11, Comm: claudeComm, Args: []string{claudeComm, "--resume", "s1"}},
		12: {PID: 12, Comm: claudeComm, Args: []string{claudeComm, sessionIDFlag, "s2"}},
		13: {PID: 13, Comm: "zsh", Args: []string{"zsh", sessionIDFlag, "s1"}},
	}}
	if got := turnPIDs(tb, "s1"); len(got) != 2 || got[0] != 10 || got[1] != 11 {
		t.Errorf("turnPIDs = %v, want [10 11]", got)
	}
}

// verbs counts the events of verb in w's log.
func verbs(t *testing.T, w *watcher, verb string) int {
	t.Helper()
	evs, err := w.store.Events(0, func(e state.Event) bool { return e.Verb == verb })
	if err != nil {
		t.Fatal(err)
	}
	return len(evs)
}

func TestReporterEndsATurnThatEndedWithoutAPost(t *testing.T) {
	w, _, out := reportingWatch(t, t.TempDir())
	ctx := context.Background()
	w.tendReporter(ctx, nil)
	w.turnEnded = func(context.Context, string) bool { return true }
	w.now = reportNow.Add(2 * time.Minute)
	w.tendReporter(ctx, nil)
	st, _ := w.store.Read()
	if st.Report.Running() || st.Report.Outcome != "unposted" || len(st.Agents) != 0 || verbs(t, w, "reporter.unposted") != 1 {
		t.Errorf("report %+v, agents %+v; out:\n%s", st.Report, st.Agents, out)
	}
}

func TestStoppedAgentsLeaveTheRunningReporterOut(t *testing.T) {
	w, _, out := reportingWatch(t, t.TempDir())
	rp := state.Party{Session: "r-1", Name: "Status report 23:00"}
	st := &state.State{Agents: []state.Agent{{Party: rp, Task: "Hourly status"}}, Report: &state.Report{Party: rp, Started: reportNow}}
	w.stoppedAgents(st, nil)
	if out.Len() != 0 {
		t.Errorf("the running reporter said stopped:\n%s", out)
	}
}

func TestReportSettings(t *testing.T) {
	var s struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Type, Command string
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(reportSettings("/bin/bee keeper")), &s); err != nil {
		t.Fatal(err)
	}
	pre := s.Hooks.PreToolUse
	if len(pre) != 1 || !regexp.MustCompile("^(?:"+pre[0].Matcher+")$").MatchString("mcp__claude_ai_Slack__slack_send_message") ||
		len(pre[0].Hooks) != 1 || pre[0].Hooks[0].Command != "'/bin/bee keeper' hook reportcheck" {
		t.Errorf("settings = %+v", s)
	}
}

func TestReporterFinalThenPaused(t *testing.T) {
	dir := t.TempDir()
	w, launched, out := reportingWatch(t, dir)
	ctx := context.Background()
	w.tendReporter(ctx, nil) // 02:00 EEST
	w.turnEnded = func(context.Context, string) bool { return true }
	w.now = reportNow.Add(time.Minute)
	w.tendReporter(ctx, nil) // ended
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.ReportPause = &state.ReportPause{Final: reportNow.Add(45 * time.Minute)}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	var prompt string
	w.runReport = func(_, _, name, p string) error {
		*launched = append(*launched, name)
		prompt = p
		return nil
	}
	w.now = reportNow.Add(30 * time.Minute)
	w.tendReporter(ctx, nil)
	w.now = reportNow.Add(45 * time.Minute)
	w.tendReporter(ctx, nil)
	if len(*launched) != 2 || (*launched)[1] != "Status report 02:45" || !strings.Contains(prompt, "Report 02:00–02:45 EEST") ||
		!strings.Contains(prompt, "last report before the 1h00m reports pause") {
		t.Fatalf("launched %v, prompt %q; out:\n%s", *launched, prompt, out)
	}
	w.tendReporter(ctx, nil) // ended
	w.now = reportNow.Add(3 * time.Hour)
	w.tendReporter(ctx, nil)
	w.tendReporter(ctx, nil)
	st, _ := w.store.Read()
	if len(*launched) != 2 || !st.ReportPause.Paused() || verbs(t, w, "reporter.pause") != 1 {
		t.Errorf("after the final: launched %v, pause %+v", *launched, st.ReportPause)
	}
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) { st.ReportPause = nil; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	w.tendReporter(ctx, nil)
	if len(*launched) != 3 {
		t.Errorf("after resume: launched %v", *launched)
	}
}
