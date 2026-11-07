package cmd

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/peer"
	"github.com/giantswarm/beekeeper/internal/state"
)

const continuityGuideSession = "continuity-guide"

func guideWorkRun(t *testing.T, a *app, args ...string) error {
	t.Helper()
	c := a.guideWorkCmd()
	c.SetArgs(args)
	c.SetOut(a.out)
	c.SilenceUsage = true
	c.SilenceErrors = true
	return c.Execute()
}

func TestGuideWorkSurvivesRelayAndRequiresTimedDeferral(t *testing.T) {
	a := guideNextApp(t)
	if err := guideWorkRun(t, a, "add", "restore the temporary fixture", "--kind", "restore", "--due", "now"); err != nil {
		t.Fatal(err)
	}
	if err := guideWorkRun(t, a, "defer", "1", "--due", "30m", "--reason", "waiting on the external proof"); err != nil {
		t.Fatal(err)
	}
	if err := guideWorkRun(t, a, "add", "finish the running goal", "--kind", guideWorkGoal, "--due", "1h"); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	if len(st.GuideWork) != 2 || st.GuideWork[0].Kind != "restore" || !st.GuideWork[0].Due.Equal(a.now.Add(30*time.Minute)) {
		t.Fatalf("owned work: %+v", st.GuideWork)
	}
	// A successor gets the same owned state and can settle the predecessor's work.
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide.Run++
		st.Guide.Holder = &state.Supervisor{Party: state.Party{Name: "Successor guide", Session: "successor-guide"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	a.as = ""
	t.Setenv("CLAUDE_CODE_SESSION_ID", "successor-guide")
	t.Setenv("CLAUDE_CODE_SESSION_NAME", "Successor guide")
	st, _ = a.store.Read()
	a.cfg.Guide.Skill = guideName
	if err := a.printGuidePrompt(st.GuideRole(), nil, "", nil, st); err != nil {
		t.Fatal(err)
	}
	text := a.out.(*bytes.Buffer).String()
	for _, want := range []string{"restore the temporary fixture", "finish the running goal", "owner: guide", "due:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("handover missing %q: %s", want, text)
		}
	}
	if err := guideWorkRun(t, a, "done", "1", "--reason", "restored and checked"); err != nil {
		t.Fatal(err)
	}
	st, _ = a.store.Read()
	if len(st.GuideWork) != 1 {
		t.Fatal("work not closed")
	}
	if why := guideStopReason(st, "successor-guide", a.now); why == "" {
		t.Fatal("turn ended without checking owned work")
	}
	if err := guideWorkRun(t, a, guideWorkCheck); err != nil {
		t.Fatal(err)
	}
	st, _ = a.store.Read()
	if why := guideStopReason(st, "successor-guide", a.now); why != "" {
		t.Fatal(why)
	}
}

func TestGuideStopRefusesDueWorkButAllowsTimedAndExternalWork(t *testing.T) {
	now := time.Now()
	st := &state.State{Guide: &state.Role{Holder: &state.Supervisor{Party: state.Party{Session: continuityGuideSession}}}, GuideWork: []state.GuideWork{{ID: 7, Kind: paramTask, Due: now}}}
	if guideStopReason(st, continuityGuideSession, now) == "" {
		t.Fatal("due work not flagged")
	}
	if guideStopReason(st, "worker", now) != "" {
		t.Fatal("blocked a worker")
	}
	st.GuideWork[0].Due = now.Add(time.Hour)
	st.Guide.WorkChecked = &state.Party{Session: continuityGuideSession}
	if guideStopReason(st, continuityGuideSession, now) != "" {
		t.Fatal("blocked timed work")
	}
}

func TestIndependentGuideMonitorRetainsAndDeliversWithoutGuideWatch(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	w.now = relayNow
	guide := state.Party{Session: "guide-without-monitor", Name: "Guide fixture"}
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.GuideReportsSince = w.now.Add(-time.Minute)
		st.Guide = &state.Role{Holder: &state.Supervisor{Party: guide, Since: w.now.Add(-time.Hour)}}
		return []state.Event{{At: w.now, Verb: loopFileVerb, Detail: "[fixture-report] issue to file" + fileReportSeparator + "the full problem"}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	var sent []string
	fail := true
	w.stand = standbyWatch{send: func(_ context.Context, _, msg string) error {
		sent = append(sent, msg)
		if fail {
			return errors.New("fixture unavailable")
		}
		return nil
	}}
	for round := 0; round < 3; round++ {
		st, _ := w.store.Read()
		w.guideMonitor(context.Background(), st, []*claude.Session{{ID: guide.Session, Name: guide.Name}})
		w.stand.inflight.Wait()
		fail = false
	}
	st, _ := w.store.Read()
	if len(st.GuideWork) != 1 || st.GuideWork[0].Text != "issue to file"+fileReportSeparator+"the full problem" {
		t.Fatalf("lost report: %+v", st.GuideWork)
	}
	if len(sent) != 2 || !strings.Contains(sent[1], "GUIDE WATCH LAPSED") || !strings.Contains(sent[1], "the full problem") {
		t.Fatalf("delivery/retry: %q", sent)
	}
	if !strings.Contains(out.String(), "GUIDE WATCH LAPSED") {
		t.Fatal("no independent lapse warning")
	}
}

func TestGuideFeedRetainsWorkBeforeSayingAProblem(t *testing.T) {
	st := &state.State{Guide: &state.Role{Holder: &state.Supervisor{Party: state.Party{Session: continuityGuideSession}}}}
	ev := state.Event{At: relayNow, Verb: loopFileVerb, Detail: "[new-problem] a finding" + fileReportSeparator + "file this"}
	lines, changed := guideProblemLines(st, []state.Event{ev})
	if !changed || len(lines) != 1 || len(st.GuideWork) != 1 || len(st.GuideReports) != 1 {
		t.Fatalf("problem not retained: %q %+v", lines, st.GuideWork)
	}
	if why := guideStopReason(st, continuityGuideSession, relayNow); why == "" {
		t.Fatal("guide can end with its new finding unfiled")
	}
	guideProblemLines(st, []state.Event{ev})
	if len(st.GuideWork) != 1 {
		t.Fatal("problem duplicated")
	}
}

func TestGuideTurnHookRequiresAWorkCheckAndResetsItOnANewPrompt(t *testing.T) {
	a, out := stubApp(t)
	a.cfgPath = filepath.Join(a.cfg.StateDir, "config.yaml")
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide.WorkChecked = &state.Party{Session: "s1"}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	run := func(event string) string {
		t.Helper()
		out.Reset()
		c := a.relayHookCmd()
		c.SetIn(strings.NewReader(`{"session_id":"s1","hook_event_name":"` + event + `"}`))
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if got := run(guideStopEvent); got != "" {
		t.Fatalf("checked turn blocked: %s", got)
	}
	if got := run("UserPromptSubmit"); got != "" {
		t.Fatalf("unexpected prompt output: %s", got)
	}
	if got := run(guideStopEvent); !strings.Contains(got, `"decision":"block"`) || !strings.Contains(got, "guide work check") {
		t.Fatalf("unchecked work not flagged: %s", got)
	}
}

func TestGuidePeerTurnInvalidatesWorkCheck(t *testing.T) {
	for _, event := range []string{guideStopEvent, guard.PreToolUseEvent, "guard-context"} {
		t.Run(event, func(t *testing.T) {
			a, out := stubApp(t)
			a.cfgPath = filepath.Join(a.cfg.StateDir, "config.yaml")
			check := func() {
				t.Helper()
				if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.Guide.WorkChecked = &state.Party{Session: "s1"}
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			run := func(event string) string {
				t.Helper()
				out.Reset()
				c := a.relayHookCmd()
				c.SetIn(strings.NewReader(`{"session_id":"s1","hook_event_name":"` + event + `"}`))
				if err := c.Execute(); err != nil {
					t.Fatal(err)
				}
				return out.String()
			}
			check()
			if got := run("Stop"); got != "" {
				t.Fatalf("checked turn blocked: %s", got)
			}
			if _, err := peer.QueueInstruction(a.cfg.StateDir, "s1", "file the new problem"); err != nil {
				t.Fatal(err)
			}
			var got string
			if event == "guard-context" {
				got = string(peer.WithInstructionContext(a.cfg.StateDir, []byte(`{"session_id":"s1"}`), nil))
			} else {
				got = run(event)
			}
			if !strings.Contains(got, "file the new problem") {
				t.Fatalf("instruction not delivered: %s", got)
			}
			if got := run("Stop"); !strings.Contains(got, `"decision":"block"`) || !strings.Contains(got, "guide work check") {
				t.Fatalf("peer turn ended without fresh work check: %s", got)
			}
			check()
			if got := run("Stop"); got != "" {
				t.Fatalf("freshly checked turn blocked: %s", got)
			}
		})
	}
}

func TestGuideReportsStartNowAndRetainBoundaryAcrossFeeds(t *testing.T) {
	now := relayNow
	problem := func(at time.Time, key string) state.Event {
		return state.Event{At: at, Verb: loopFileVerb, Detail: "[" + key + "] finding" + fileReportSeparator + "details"}
	}
	st := &state.State{}
	old := problem(now.Add(-time.Hour), "old")
	if got := guideReportEvents(st, []state.Event{old}, now); len(got) != 0 {
		t.Fatal("replayed pre-upgrade history")
	}
	fresh := problem(now.Add(time.Minute), "fresh")
	if got := guideReportEvents(st, []state.Event{old, fresh}, now.Add(time.Minute)); len(got) != 1 {
		t.Fatalf("lost fresh report: %+v", got)
	}
	if got := guideReportEvents(st, []state.Event{old}, now.Add(2*time.Minute)); len(got) != 0 {
		t.Fatal("replayed history on another feed")
	}
	st.GuideWork = []state.GuideWork{
		{ID: 1, Source: "old-a", Due: now.Add(-48 * time.Hour), Text: "historical problem alpha"},
		{ID: 2, Source: "old-b", Due: now.Add(-48 * time.Hour), Text: "historical problem beta"},
		{ID: 3, Due: now.Add(-48 * time.Hour), Text: "explicit guide task"},
	}
	guideReportEvents(st, nil, now)
	if len(st.GuideWork) != 2 || st.GuideWork[1].Kind != guideWorkBacklog || guideWorkBlocks(st.GuideWork[1], now) || !guideWorkBlocks(st.GuideWork[0], now) {
		t.Fatalf("backlog migration: %+v", st.GuideWork)
	}
	text := st.GuideWork[1].Text
	guideReportEvents(st, nil, now)
	if st.GuideWork[1].Text != text {
		t.Fatal("backlog migration is not idempotent")
	}
}
