package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

func noteCommand(a *app, args ...string) (string, error) {
	var errOut bytes.Buffer
	c := a.noteCmd()
	c.SetArgs(args)
	c.SetOut(a.out)
	c.SetErr(&errOut)
	c.SilenceUsage, c.SilenceErrors = true, true
	err := c.Execute()
	return errOut.String(), err
}

func TestSplitNotesLeavesTheGuidesNotesToALine(t *testing.T) {
	a := &app{cfg: &config.Config{Guide: config.Guide{Person: "Pat"}}}
	s := a.splitNotes([]state.Note{
		{ID: 1, For: "pat", Text: "approve"},
		{ID: 2, For: "Guide", Text: "archive the test sessions"},
		{ID: 3, For: "Guide run 2: Pat's open decisions", Text: "relay"},
		{ID: 4, Text: "[for Pat] an older note"},
		{ID: 5, For: "Supervisor", Text: "check the lane"},
		{ID: 6, Text: "a memo"},
		{ID: 7, For: "Pat", Text: "report to Pat by name", Pinned: true},
	})
	if len(s.Own) != 2 || s.Own[0].ID != 5 || s.Own[1].ID != 6 || len(s.Pinned) != 1 || s.Pinned[0].ID != 7 || s.guided() != 4 {
		t.Fatalf("split %+v", s)
	}
	if got, want := guidedText(s), "4 notes the guide serves (1 for Guide, 1 for Guide run 2: Pat's open decisions, 2 for Pat)"; !strings.HasPrefix(got, want) {
		t.Errorf("line %q, want %q…", got, want)
	}
}

func TestLiveRecordsDropsSessionsEndedOverAnHourAgo(t *testing.T) {
	a := &app{now: relayNow}
	live := []*claude.Session{{ID: "s4", HostID: hostFour, Name: agentFour}}
	kept, dropped := a.liveRecords([]state.Record{
		{Session: four, Issue: issue},
		{Session: gone, Issue: issue, Ended: relayNow.Add(-10 * time.Minute)},
		{Session: state.Party{Session: "s8", Name: "old"}, Issue: issue, Ended: relayNow.Add(-26 * time.Hour)},
		{Session: state.Party{Session: "s7", Name: "unseen"}, Issue: issue},
	}, live)
	if len(kept) != 3 || dropped != 1 || kept[2].Session.Name != "unseen" {
		t.Fatalf("kept %+v, dropped %d", kept, dropped)
	}
	if got := droppedText(1); !strings.HasPrefix(got, "1 records of sessions ended over 1h ago") {
		t.Errorf("line %q", got)
	}
}

func TestAnswersSinceTheLastRelayAndWithin72h(t *testing.T) {
	a, out := noteApp(t)
	a.now = time.Now()
	sup := state.Party{Session: "s1", Name: "Supervisor run 3"}
	log := func(ago time.Duration, verb, detail string) {
		if err := a.store.Log(state.Event{At: a.now.Add(-ago).UTC(), By: sup, Verb: verb, Detail: detail}); err != nil {
			t.Fatal(err)
		}
	}
	log(80*time.Hour, "note.answered", "#1 answered for Pat: too old (asked by A: approve x)")
	log(5*time.Hour, "note.answered", "#2 answered for Pat: Yes, ship it (asked by Agent two: approve "+prURL+" Status quo: green.)")
	log(3*time.Hour, "supervisor.start", "run 2")
	log(2*time.Hour, "note.answered", "#3 answered for Pat: No (asked by B: merge y)")
	log(time.Hour, "supervisor.start", "run 3")
	st := &state.State{Supervisor: &state.Supervisor{Party: sup, Since: a.now.Add(-time.Hour)}}
	ans, err := a.handoverAnswers(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.All) != 2 || len(ans.Recent()) != 1 || ans.Recent()[0].ID != 3 || ans.Recent()[0].Answer != "No" || ans.All[0].For != "Pat" {
		t.Fatalf("answers %+v", ans)
	}
	a.printAnswers(ans, false)
	if got := out.String(); !strings.Contains(got, `#3 (for Pat, answered`) || !strings.Contains(got, `"No" on: merge y`) || !strings.Contains(got, "1 more answered within 72h") {
		t.Errorf("printed:\n%s", got)
	}

	// A new note asking #2 again is filed with the warning.
	stderr, err := noteCommand(a, "add", "--for", "Pat", "--status-quo", sqNow, "--why", whyNow, "--default", dfltOK, "Approve "+prURL)
	if err != nil {
		t.Fatal(err)
	}
	if want := `warning: note #2 asked approve on o/r#7 and was answered`; !strings.Contains(stderr, want) || !strings.Contains(stderr, `"Yes, ship it"`) {
		t.Errorf("stderr %q, want %q", stderr, want)
	}
	if stderr, _ := noteCommand(a, "add", "--for", "Pat", "--status-quo", sqNow, "--why", whyNow, "--default", dfltOK, "review "+prURL); stderr != "" {
		t.Errorf("another verb warned: %q", stderr)
	}
}

func TestNoteAnsweredDetailParsesBack(t *testing.T) {
	a, _ := noteApp(t)
	if err := addNote(a, "a memo on the lane"); err != nil {
		t.Fatal(err)
	}
	if _, err := noteCommand(a, "answer", "1", "keep it", "(for now)"); err != nil {
		t.Fatal(err)
	}
	all, err := a.answeredSince(time.Time{})
	if err != nil || len(all) != 1 {
		t.Fatalf("%+v, %v", all, err)
	}
	if an := all[0]; an.ID != 1 || an.For != "nobody named" || an.Answer != "keep it (for now)" || an.Asker != agentOne || an.Question != "a memo on the lane" {
		t.Errorf("parsed %+v", an)
	}
}

func TestPinnedNoteStaysInEveryHandoverUntilUnpinned(t *testing.T) {
	a, out := noteApp(t)
	if err := addNote(a, "--pin", "report every merge with its release"); err != nil {
		t.Fatal(err)
	}
	if err := addNote(a, "--for", "Guide", "archive the test sessions"); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	a.cfg.Supervisor.Skill, a.cfg.Guide.Skill, a.cfg.StateDir = "supervise", "guide", t.TempDir()
	out.Reset()
	if err := a.printPrompt(context.Background(), &view{st: st}, &leaseList{}, &alertsView{}, answers{}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "### Standing instructions (pinned notes)\n\n- #1: report every merge") || strings.Contains(got, "archive the test sessions") ||
		!strings.Contains(got, "- 1 notes the guide serves (1 for Guide)") {
		t.Errorf("supervisor prompt:\n%s", got)
	}
	out.Reset()
	if err := a.printGuidePrompt(state.Role{}, nil, a.splitNotes(st.Notes).Pinned); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "- #1: report every merge") {
		t.Errorf("guide prompt:\n%s", out.String())
	}
	if _, err := noteCommand(a, "unpin", "#1"); err != nil {
		t.Fatal(err)
	}
	if _, err := noteCommand(a, "pin", "9"); Code(err) != ExitRefused {
		t.Errorf("pinning a closed note: %v", err)
	}
	st, _ = a.store.Read()
	if s := a.splitNotes(st.Notes); len(s.Pinned) != 0 || len(s.Own) != 1 {
		t.Errorf("after unpin: %+v", s)
	}
}

func TestBroadcastReachesEveryRunningAgentOnce(t *testing.T) {
	sup := state.Party{Session: "s0", Name: "Supervisor run 3"}
	agents := []state.Agent{
		{Party: sup},
		{Party: four},
		{Party: state.Party{Session: "s5", Name: "Agent five"}},
		{Party: state.Party{Session: "s6", Name: "Agent six"}},
		{Party: gone},
	}
	sessions := []*claude.Session{
		{ID: "s0", Name: sup.Name}, {ID: "s4", HostID: hostFour, Name: agentFour},
		{ID: "s5", Name: "Agent five"}, {ID: "s6", Name: "Agent six"},
	}
	var sent []string
	told, missed := broadcast(context.Background(), agents, sessions, []state.Party{{Name: sup.Name}}, func(_ context.Context, name string) error {
		sent = append(sent, name)
		if name == "Agent six" {
			return errors.New("unreachable")
		}
		return nil
	})
	if fmt.Sprint(told) != "[Agent four Agent five]" || len(missed) != 1 || !strings.HasPrefix(missed[0], "Agent six: unreachable") || len(sent) != 3 {
		t.Fatalf("told %v, missed %v, sent %v", told, missed, sent)
	}
}

func TestStartSummaryFitsInFiveKB(t *testing.T) {
	a, out := noteApp(t)
	a.cfg.LeaseDir = t.TempDir()
	st := &state.State{Supervisor: &state.Supervisor{Party: state.Party{Session: "s0", Name: "Supervisor run 3"}, Since: relayNow}}
	for i := range 300 {
		st.Notes = append(st.Notes, state.Note{ID: i, For: "Pat", Text: strings.Repeat("a long question ", 30)})
	}
	for i := range 30 {
		st.Agents = append(st.Agents, state.Agent{Party: state.Party{Session: fmt.Sprint("s", i), Name: fmt.Sprintf("Board pull %d with a long title", i)}, Task: strings.Repeat("giantswarm/beekeeper#237 ", 10)})
		st.Records = append(st.Records, state.Record{Session: state.Party{Name: fmt.Sprint("r", i)}, Issue: issue, Ended: relayNow.Add(-48 * time.Hour)})
	}
	if err := a.startSummary(st, nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if len(got) >= 5*1024 || !strings.Contains(got, "300 the guide serves") || !strings.Contains(got, "(its CLI is not running)") || !strings.Contains(got, "--section <name>") {
		t.Errorf("%d bytes:\n%s", len(got), got)
	}
}
