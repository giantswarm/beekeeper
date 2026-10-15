package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	pat       = "Pat"
	supRun3   = "Supervisor run 3"
	agentFive = "Agent five"
	agentSix  = "Agent six"
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
	a := &app{cfg: &config.Config{Guide: config.Guide{Person: pat}}}
	s := a.splitNotes([]state.Note{
		{ID: 1, Kind: noteDecision, For: "pat", Text: "approve"},
		{ID: 2, Kind: noteDecision, For: guideRole.title, Text: "archive the test sessions"},
		{ID: 3, Kind: noteDecision, For: "Guide run 2: Pat's open decisions", Text: "relay"},
		{ID: 4, Kind: noteDecision, Text: "[for Pat] an older note"},
		{ID: 5, Kind: noteDecision, For: supervisorRole.title, Text: "check the lane"},
		{ID: 6, Kind: noteDecision, For: samName, Text: "pick a lab?"},
	})
	if len(s.Own) != 2 || s.Own[0].ID != 5 || s.Own[1].ID != 6 || s.guided() != 4 {
		t.Fatalf("split %+v", s)
	}
	if got, want := guidedText(s), "4 notes the guide serves (1 for Guide, 1 for Guide run 2: Pat's open decisions, 2 for Pat)"; !strings.HasPrefix(got, want) {
		t.Errorf("line %q, want %q…", got, want)
	}
}

func TestLiveRecordsDropsSessionsEndedOverAnHourAgo(t *testing.T) {
	a := &app{now: relayNow}
	live := []*claude.Session{{ID: "s4", HostID: hostFour, Name: agentFour}}
	kept, dropped := a.liveRecords(&state.State{}, []state.Record{
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
	sup := state.Party{Session: "s1", Name: supRun3}
	log := func(ago time.Duration, verb, detail string) {
		if err := a.store.Log(state.Event{At: a.now.Add(-ago).UTC(), By: sup, Verb: verb, Detail: detail}); err != nil {
			t.Fatal(err)
		}
	}
	log(80*time.Hour, answerRecorded, "o/r#1 answered by \"Pat\" via cli, recorded at https://c/1, note #1 asked by \"A\": \"approve x\": too old")
	log(5*time.Hour, answerRecorded, "o/r#7 answered by \"Pat\" via cli, recorded at https://c/2, note #2 asked by \"Agent two\": \"approve "+prURL+" Status quo: green.\": Yes, ship it")
	log(3*time.Hour, "supervisor.start", "run 2")
	log(2*time.Hour, answerRecorded, "o/r#8 answered by \"Pat\" via slack, recorded at https://c/3, note #3 asked by \"B\": \"merge y\": No")
	log(90*time.Minute, answerRecorded, "o/r#9 answered by \"Pat\" via cli, recorded at https://c/4: the draft stays")
	log(time.Hour, "supervisor.start", "run 3")
	st := &state.State{Supervisor: &state.Supervisor{Party: sup, Since: a.now.Add(-time.Hour)}}
	ans, err := a.handoverAnswers(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.All) != 3 || len(ans.Recent()) != 2 || ans.Recent()[0].ID != 3 || ans.Recent()[0].Answer != "No" || ans.All[0].For != pat || ans.Recent()[1].ID != 0 {
		t.Fatalf("answers %+v", ans)
	}
	a.printAnswers(ans, false)
	if got := out.String(); !strings.Contains(got, `o/r#8 (by Pat, answered`) || !strings.Contains(got, `"No" on note #3: merge y`) ||
		!strings.Contains(got, `o/r#9 (by Pat, answered`) || !strings.Contains(got, `"the draft stays"`+"\n") || !strings.Contains(got, "1 more answered within 72h") {
		t.Errorf("printed:\n%s", got)
	}

	// A new note on o/r#7 is filed with the warning quoting its answer.
	stderr, err := noteCommand(a, "add", "--for", pat, "--status-quo", sqNow, "--why", whyNow, "--default", dfltOK, "--due", "3h", "--ref", "o/r#7", "Merge "+prURL)
	if err != nil {
		t.Fatal(err)
	}
	if want := `warning: "Pat" answered on o/r#7 `; !strings.Contains(stderr, want) || !strings.Contains(stderr, `(https://c/2): "Yes, ship it"`) {
		t.Errorf("stderr %q, want %q", stderr, want)
	}
	if stderr, _ := noteCommand(a, "add", "--for", pat, "--status-quo", sqNow, "--why", whyNow, "--default", dfltOK, "--due", "3h", "--ref", "o/r#10", "review "+prURL); stderr != "" {
		t.Errorf("another thread warned: %q", stderr)
	}
}

func TestNoteAnsweredDetailParsesBack(t *testing.T) {
	a, _ := noteApp(t)
	if err := addNote(a, flagFor, samName, flagStatusQuo, sqNow, flagDefault, dfltOK, flagDue, "3h", "--ref", "o/r#15", "pick the lane?"); err != nil {
		t.Fatal(err)
	}
	if _, err := noteCommand(a, "answer", "1", "keep it", "(for now)"); err != nil {
		t.Fatal(err)
	}
	all, err := a.answeredSince(time.Time{})
	if err != nil || len(all) != 1 {
		t.Fatalf("%+v, %v", all, err)
	}
	if an := all[0]; an.ID != 1 || an.About != "o/r#15" || an.For != samName || an.Answer != "keep it (for now)" || an.Asker != agentOne || !strings.HasPrefix(an.Question, "pick the lane?") {
		t.Errorf("parsed %+v", an)
	}
}

// pinnedNotes is a state document an older beekeeper wrote with pinned
// notes, one naming an issue, one naming none over several lines, a pinned
// decision, an unpinned memo and an open decision.
const pinnedNotes = `{"nextNote": 5, "notes": [
	{"id": 1, "for": "Supervisor", "kind": "memo", "pinned": true, "text": "report every merge with its release, o/r#7", "by": {"name": "Agent one"}, "at": "2026-10-01T00:00:00Z"},
	{"id": 2, "pinned": true, "text": "the lab runs at most two kind clusters.\nAsk before a third:   memory is shared.", "by": {"name": "Guide"}, "at": "2026-10-02T00:00:00Z"},
	{"id": 3, "for": "Pat", "kind": "decision", "pinned": true, "text": "keep the nightly sweep at 02:00", "by": {"name": "Agent one"}, "at": "2026-10-03T00:00:00Z"},
	{"id": 4, "for": "Guide", "text": "archive the test sessions", "by": {"name": "Agent one"}, "at": "2026-10-01T00:00:00Z"},
	{"id": 5, "for": "Pat", "kind": "decision", "text": "merge o/r#9?", "refs": ["o/r#9"], "default": "it waits", "by": {"name": "Agent one"}, "at": "2026-10-01T00:00:00Z"}]}`

// pinTexts are the texts of pinnedNotes' pins.
var pinTexts = []string{
	"report every merge with its release, o/r#7",
	"the lab runs at most two kind clusters.\nAsk before a third:   memory is shared.",
	"keep the nightly sweep at 02:00",
}

// handoverPrompts prints the supervisor's and the guide's prompts.
func handoverPrompts(t *testing.T, a *app, out *bytes.Buffer) (sup, guide string) {
	t.Helper()
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := a.printPrompt(context.Background(), &view{st: st}, &leaseList{}, &alertsView{}, answers{}); err != nil {
		t.Fatal(err)
	}
	sup = out.String()
	s, err := a.readStanding(st)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := a.printGuidePrompt(state.Role{}, nil, s); err != nil {
		t.Fatal(err)
	}
	return sup, out.String()
}

// carried counts the pins whose whole text, spacing aside, prompt carries.
func carried(prompt string) int {
	flat := strings.Join(strings.Fields(prompt), " ")
	n := 0
	for _, p := range pinTexts {
		if strings.Contains(flat, strings.Join(strings.Fields(p), " ")) {
			n++
		}
	}
	return n
}

func TestPinnedNotesReachBothHandoversBeforeAndAfterTheMigration(t *testing.T) {
	a, out := noteApp(t)
	a.cfg.Supervisor.Skill, a.cfg.Guide.Skill, a.cfg.StateDir = "supervise", "guide", t.TempDir()
	writeState(t, a, pinnedNotes)

	sup, guide := handoverPrompts(t, a, out)
	if carried(sup) != len(pinTexts) || carried(guide) != len(pinTexts) {
		t.Fatalf("unmigrated: supervisor %d/%d, guide %d/%d pins:\n%s\n---\n%s", carried(sup), len(pinTexts), carried(guide), len(pinTexts), sup, guide)
	}
	for _, p := range []string{sup, guide} {
		if !strings.Contains(p, "### Standing instructions") || strings.Contains(p, "archive the test sessions") {
			t.Errorf("the pins are standing instructions, the memo none:\n%s", p)
		}
	}

	if _, err := noteCommand(a, "migrate", "--apply"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "closed 4 memos and pinned notes; 3 pins moved into ") {
		t.Errorf("apply: %q", out.String())
	}
	if st, _ := a.store.Read(); len(st.Memos) != 0 || len(st.Notes) != 1 {
		t.Fatalf("every memo and pin closed: notes %+v, memos %+v", st.Notes, st.Memos)
	}
	raw, err := os.ReadFile(a.standingPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pinTexts {
		if !strings.Contains(string(raw), p) {
			t.Errorf("%s lacks %q verbatim:\n%s", standingFile, p, raw)
		}
	}
	if !strings.Contains(string(raw), "#### #2 (by \"Guide\", pinned 2026-10-02)\n\nthe lab runs") {
		t.Errorf("a pin's block names it:\n%s", raw)
	}

	sup, guide = handoverPrompts(t, a, out)
	if carried(sup) != len(pinTexts) || carried(guide) != len(pinTexts) {
		t.Fatalf("migrated: supervisor %d/%d, guide %d/%d pins:\n%s\n---\n%s", carried(sup), len(pinTexts), carried(guide), len(pinTexts), sup, guide)
	}
	if strings.Contains(sup, unmovedPins) {
		t.Errorf("no pin is left to move:\n%s", sup)
	}

	// A second migration finds nothing and the file keeps one block a pin.
	writeState(t, a, pinnedNotes)
	if _, err := noteCommand(a, "migrate", "--apply"); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(a.standingPath()); string(again) != string(raw) {
		t.Errorf("a pin moved twice:\n%s", again)
	}
}

func TestNoteMigrateClosesNoPinItCannotMove(t *testing.T) {
	a, out := noteApp(t)
	a.cfg.StateDir = t.TempDir()
	writeState(t, a, pinnedNotes)
	if err := os.Mkdir(a.standingPath(), 0o700); err != nil { // standing.md cannot be written
		t.Fatal(err)
	}
	_, err := noteCommand(a, "migrate", "--apply")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "closed nothing: pinned note #1, #2, #3 not moved") {
		t.Fatalf("migrate into an unwritable file: %v (%s)", err, out.String())
	}
	if st, _ := a.store.Read(); len(st.Memos) != 4 {
		t.Fatalf("every memo and pin stays open: %+v", st.Memos)
	}
	if _, err := noteCommand(a, "migrate", "--apply", "4"); err != nil {
		t.Fatalf("a memo without a pin closes: %v", err)
	}
}

func TestBroadcastReachesEveryRunningAgentOnce(t *testing.T) {
	sup := state.Party{Session: "s0", Name: supRun3}
	agents := []state.Agent{
		{Party: sup},
		{Party: four},
		{Party: state.Party{Session: "s5", Name: agentFive}},
		{Party: state.Party{Session: "s6", Name: agentSix}},
		{Party: gone},
	}
	sessions := []*claude.Session{
		{ID: "s0", Name: sup.Name}, {ID: "s4", HostID: hostFour, Name: agentFour},
		{ID: "s5", Name: agentFive}, {ID: "s6", Name: agentSix},
	}
	var sent []string
	told, missed := broadcast(context.Background(), agents, sessions, []state.Party{{Name: sup.Name}}, func(_ context.Context, name string) error {
		sent = append(sent, name)
		if name == agentSix {
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
	st := &state.State{Supervisor: &state.Supervisor{Party: state.Party{Session: "s0", Name: supRun3}, Since: relayNow}}
	for i := range 300 {
		st.Notes = append(st.Notes, state.Note{ID: i, Kind: noteDecision, For: pat, Text: strings.Repeat("a long question ", 30)})
	}
	for i := range 30 {
		st.Agents = append(st.Agents, state.Agent{Party: state.Party{Session: fmt.Sprint("s", i), Name: fmt.Sprintf("Board pull %d with a long title", i)}, Task: strings.Repeat("giantswarm/beekeeper#237 ", 10)})
		st.Records = append(st.Records, state.Record{Session: state.Party{Name: fmt.Sprint("r", i)}, Issue: issue, Ended: relayNow.Add(-48 * time.Hour)})
	}
	if err := a.startSummary(st, nil, &leaseList{Free: []string{graveler}}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if len(got) >= 5*1024 || !strings.Contains(got, "300 the guide serves") || !strings.Contains(got, "(its CLI is not running)") || !strings.Contains(got, "--section <name>") {
		t.Errorf("%d bytes:\n%s", len(got), got)
	}
}

func TestPromptKeepsAParkedWorkerUntilItsParkResolves(t *testing.T) {
	ended := relayNow.Add(-2 * time.Hour)
	park := &state.Park{On: "#21", Waits: "the person's word on the threshold", At: ended}
	record := state.Record{Session: gone, Issue: issue, Waits: "a GPU node", Ended: ended}
	agent := state.Agent{Party: gone, Task: issue, AssignedAt: ended.Add(-time.Hour)}
	for _, tc := range []struct {
		name   string
		agent  func(state.Agent) []state.Agent
		record string
	}{
		{"parked on a note", func(ag state.Agent) []state.Agent { ag.Park = park; return []state.Agent{ag} },
			`- "Continue #37639" parked, no CLI, parked on #21: the person's word on the threshold; it serves ` + issue + ", waiting on a GPU node"},
		{"park resolved", func(ag state.Agent) []state.Agent { return []state.Agent{ag} }, ""},
		{"done with its task", func(ag state.Agent) []state.Agent { ag.Park, ag.Done = park, true; return []state.Agent{ag} }, ""},
		{"archived", func(state.Agent) []state.Agent { return nil }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &state.State{Records: []state.Record{record}, Agents: tc.agent(agent)}
			var out bytes.Buffer
			a := &app{out: &out, now: relayNow}
			p := func(format string, args ...any) { _, _ = fmt.Fprintf(&out, format+"\n", args...) }
			a.promptRecords(p, st, nil)
			got := out.String()
			if tc.record == "" {
				if strings.Contains(got, gone.Name) || !strings.Contains(got, droppedText(1)) {
					t.Errorf("not dropped:\n%s", got)
				}
				return
			}
			if !strings.Contains(got, tc.record+"\n") || strings.Contains(got, "has ended") || strings.Contains(got, "records of sessions ended") {
				t.Errorf("prompt lacks %q:\n%s", tc.record, got)
			}
		})
	}
}

func TestPromptAgentsSaysAParkedAgentsParkAlone(t *testing.T) {
	var out bytes.Buffer
	a := &app{out: &out, now: relayNow}
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(&out, format+"\n", args...) }
	ag := state.Agent{Party: gone, Task: "review the plan", AssignedAt: relayNow.Add(-3 * time.Hour), Park: &state.Park{Waits: "a colleague's review"}}
	a.promptAgents(p, []state.Agent{ag}, nil)
	if got := out.String(); !strings.Contains(got, `"Continue #37639" on review the plan since`) || !strings.Contains(got, ", parked: a colleague's review\n") || strings.Contains(got, "not running") {
		t.Errorf("agents:\n%s", got)
	}
}
