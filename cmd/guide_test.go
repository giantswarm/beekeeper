package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestGuideRelayDueFiresOncePerTerm(t *testing.T) {
	st := &state.State{}
	guideRole.set(st, state.Role{Holder: &state.Supervisor{Party: agentC, Since: relayNow}})
	lines, evs := guideRole.fireRelayDue(st, quietness{checked: true, context: 50_000, relayAt: 40_000}, nil, relayNow)
	if len(lines) != 1 || lines[0] != `GUIDE RELAY DUE: "Agent three" is at 50k tokens of context: beekeeper guide handover --prompt` || evs[0].Verb != "guide.relay-due" {
		t.Fatalf("relay due: %q %v", lines, evs)
	}
	if st.Guide.RelayDue == nil {
		t.Fatal("the guide's relay due is not recorded")
	}
	if lines, _ := guideRole.fireRelayDue(st, quietness{checked: true, context: 60_000, relayAt: 40_000}, nil, relayNow); lines != nil {
		t.Fatalf("relay due said twice: %q", lines)
	}
}

func TestGuideFeedSaysEachItemOnce(t *testing.T) {
	owner := state.Party{Session: "sO", Name: "Agent seven"}
	a := &app{now: relayNow, cfg: &config.Config{}} // guide.person unset: every --for note
	st := &state.State{Notes: []state.Note{
		{ID: 1, Kind: noteDecision, For: "Alex", Text: "merge the bump?", Default: "it waits", By: owner},
		{ID: 2, Kind: noteDecision, Text: "check the rollout", By: owner},
	}}
	agent := state.Party{Session: "sC", Name: agentC.Name}
	st.Records = []state.Record{{Session: agent, Issue: refOne, Waits: personTimo + ": " + approveADR}}
	sessions := []*claude.Session{{ID: "sC", Name: agentC.Name}, {ID: "sO", Name: "Agent seven"}}
	lines, changed := a.feedLines(st, sessions, nil, nil)
	if !changed || len(lines) != 2 ||
		lines[0] != `GUIDE DECISION: #1 for Alex from "Agent seven": merge the bump?; if unanswered: it waits` ||
		lines[1] != `GUIDE WAITING: "Agent three" needs its person: Alex: approve the ADR` {
		t.Fatalf("first poll: %q", lines)
	}
	if lines, changed := a.feedLines(st, sessions, nil, nil); changed || lines != nil {
		t.Fatalf("second poll said again: %q", lines)
	}
	st.Notes = st.Notes[1:]
	st.Records[0].Waits = "Alex: pick a name"
	closed := map[int]state.Event{1: {Verb: answerRecorded, By: state.Party{Name: "Guide"}, Detail: refOne + " answered by \"Alex\" via cli, recorded at https://c/1, note #1 asked by \"Agent seven\": \"merge the bump?\": yes, merge it"}}
	lines, _ = a.feedLines(st, sessions, closed, nil)
	if len(lines) != 2 || lines[0] != `GUIDE WAITING: "Agent three" needs its person: Alex: pick a name` ||
		lines[1] != "GUIDE ANSWERED (Guide): "+refOne+" answered by \"Alex\" via cli, recorded at https://c/1, note #1 asked by \"Agent seven\": \"merge the bump?\": yes, merge it" {
		t.Fatalf("answer and new wait: %q", lines)
	}
}

const (
	approveADR = "approve the ADR"
	personTimo = "Alex"
	ciWait     = "CI on #12"
)

func TestDesktopRecordSaysWaiting(t *testing.T) {
	var r claude.Record
	raw := `{"postTurnSummary":{"status_category":"blocked","needs_action":"approve #147"},"postTurnSummaryFor":"u2","lastAssistantUuid":"u2"}`
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	if w := r.Waiting(); w == nil || w.Action != "approve #147" || w.Turn != "u2" {
		t.Fatalf("blocked summary of the latest turn: %+v", w)
	}
	r.LastAssistantUUID = "u3" // a newer turn: the summary is stale
	if r.Waiting() != nil {
		t.Fatal("a stale summary says waiting")
	}
	r.LastAssistantUUID, r.PostTurnSummary.Category = "u2", "review_ready"
	if r.Waiting() != nil {
		t.Fatal("a review_ready summary says waiting")
	}
}

// guideNotes are the shapes of a live state's notes: filed for the person
// (Pat, in any case, one only by its older "[for Pat]" prefix), for the
// supervisor, for a team and for nobody.
func guideNotes(t *testing.T) *state.State {
	t.Helper()
	raw, err := os.ReadFile("testdata/guide-notes.json")
	if err != nil {
		t.Fatal(err)
	}
	st := &state.State{}
	if err := json.Unmarshal(raw, &st.Notes); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestGuideQueueHoldsOnlyItsPersonsNotes(t *testing.T) {
	st := guideNotes(t)
	var out strings.Builder
	a := &app{out: &out, now: relayNow}
	sessions := []*claude.Session{{ID: "s1", Name: agentOne}, {ID: "s3", Name: agentC.Name}}
	st.Records = []state.Record{
		{Session: state.Party{Session: "s3", Name: agentC.Name}, Waits: "pat: " + approveADR},
		{Session: state.Party{Session: "s1", Name: agentOne}, Waits: "Supervisor: the lease"},
	}
	q, _ := guideQueue(st, sessions, "Pat", time.Time{})
	a.printQueue(q)
	want := `#84 for Pat from "Agent three": refine the proposal: the split or one issue?
#99 for Pat from "Agent three": merge the context bump?; if unanswered: it waits
#100 for pat from "Agent one": relay due: pick the successor
"Agent three" waits on its person: approve the ADR
`
	if out.String() != want {
		t.Fatalf("queue for Pat:\n%s\nwant:\n%s", out.String(), want)
	}
	var ids []int
	all, _ := guideQueue(st, sessions, "", time.Time{})
	for _, it := range all {
		if it.Note != nil {
			ids = append(ids, it.Note.ID)
		}
	}
	if !slices.Equal(ids, []int{23, 37, 88, 99, 100}) {
		t.Fatalf("with guide.person unset, the queue holds every --for note as before: %v", ids)
	}
}

func TestGuideFeedSaysOnlyItsPersonsNotesOnce(t *testing.T) {
	st := guideNotes(t)
	a := &app{now: relayNow, cfg: &config.Config{Guide: config.Guide{Person: "PAT"}}}
	// A feed before guide.person said the supervisor's and the team's notes.
	guideRole.update(st, func(r *state.Role) { r.Fed = []string{"note#23", "note#37"} })
	sessions := []*claude.Session{{ID: "s1", Name: agentOne}, {ID: "s3", Name: agentC.Name}}
	lines, changed := a.feedLines(st, sessions, nil, nil)
	want := []string{
		`GUIDE DECISION: #84 for Pat from "Agent three": refine the proposal: the split or one issue?`,
		`GUIDE DECISION: #99 for Pat from "Agent three": merge the context bump?; if unanswered: it waits`,
		`GUIDE DECISION: #100 for pat from "Agent one": relay due: pick the successor`,
	}
	if !changed || !slices.Equal(lines, want) {
		t.Fatalf("first poll: %q", lines)
	}
	if fed := st.GuideRole().Fed; !slices.Equal(fed, []string{"note#100", "note#84", "note#99"}) {
		t.Fatalf("fed: %q", fed)
	}
	if lines, changed := a.feedLines(st, sessions, nil, nil); changed || lines != nil {
		t.Fatalf("second poll said again: %q", lines)
	}
	st.Notes = slices.DeleteFunc(st.Notes, func(n state.Note) bool { return n.ID == 23 || n.ID == 99 })
	closed := map[int]state.Event{99: {Verb: answerRecorded, By: state.Party{Name: "Guide"}, Detail: "o/r#99 answered by \"Pat\" via cli, recorded at https://c/99, note #99 asked by \"Agent three\": \"merge?\": yes"}}
	if lines, _ := a.feedLines(st, sessions, closed, nil); !slices.Equal(lines, []string{"GUIDE ANSWERED (Guide): o/r#99 answered by \"Pat\" via cli, recorded at https://c/99, note #99 asked by \"Agent three\": \"merge?\": yes"}) {
		t.Fatalf("the person's answer, and nothing for the supervisor's closed note: %q", lines)
	}
}

const guideHost = "local_g"

func TestGuideQueueSkipsArchivedTestAndOwnSessions(t *testing.T) {
	guide := state.Party{Session: "sG", HostSession: guideHost, Name: "Guide Alex through his decisions"}
	st := &state.State{}
	guideRole.set(st, state.Role{Holder: &state.Supervisor{Party: guide, Since: relayNow}})
	party := func(id, host, name string) state.Party {
		return state.Party{Session: id, HostSession: host, Name: name}
	}
	ask := personTimo + ": " + approveADR
	st.Records = []state.Record{
		{Session: party("sA", "local_a", "c-32"), Waits: ask},
		{Session: party("sT", "local_t", "test: beekeeper#60 permission hook"), Waits: ask},
		{Session: guide, Waits: ask},
		{Session: party("sC", "local_c", agentC.Name), Waits: ask},
		{Session: party("sS", "local_s", "Land the follow-ups"), Waits: "Alex: merge it"},
		{Session: party("sX", "local_x", "test: a stopped run"), Waits: ask},
	}
	sessions := []*claude.Session{
		{ID: "sA", HostID: "local_a", Name: "c-32", Archived: true},
		{ID: "sT", HostID: "local_t", Name: "test: beekeeper#60 permission hook"},
		{ID: "sG", HostID: guideHost, Name: guide.Name},
		{ID: "sC", HostID: "local_c", Name: agentC.Name},
	}
	var out strings.Builder
	a := &app{out: &out, now: relayNow, cfg: &config.Config{}}
	q, _ := guideQueue(st, sessions, personTimo, time.Time{})
	a.printQueue(q)
	want := `"Agent three" waits on its person: approve the ADR
"Land the follow-ups" (stopped) waits on its person: merge it
`
	if out.String() != want {
		t.Fatalf("queue:\n%s\nwant:\n%s", out.String(), want)
	}
	a.cfg.Guide.Person = personTimo
	lines, _ := a.feedLines(st, sessions, nil, nil)
	if !slices.Equal(lines, []string{
		`GUIDE WAITING: "Agent three" needs its person: approve the ADR`,
		`GUIDE WAITING: "Land the follow-ups" (stopped) needs its person: merge it`,
	}) {
		t.Fatalf("feed: %q", lines)
	}
}

func TestGuideQueueIgnoresTheDesktopsTurnSummary(t *testing.T) {
	// The supervisor's turn is a status report the desktop marks as needing
	// its person; it filed no note and serves no --waits for the person.
	sup := state.Party{Session: "sP", Name: "Supervisor run 17"}
	st := &state.State{Records: []state.Record{{Session: sup, Issue: reserveIssue}}}
	sessions := []*claude.Session{{ID: "sP", Name: sup.Name, Waiting: &claude.Waiting{Turn: "t1", Action: "clarify: is fixture-lab-67 an agent"}}}
	if q, _ := guideQueue(st, sessions, personTimo, time.Time{}); len(q) != 0 {
		t.Fatalf("a report read as an ask is queued: %+v", q)
	}
	a := &app{now: relayNow, cfg: &config.Config{Guide: config.Guide{Person: personTimo}}}
	if lines, _ := a.feedLines(st, sessions, nil, nil); lines != nil {
		t.Fatalf("a report read as an ask is fed: %q", lines)
	}
}

func TestWaitsOnReadsTheAskOfThePerson(t *testing.T) {
	for _, c := range []struct {
		person, waits, ask string
		ok                 bool
	}{
		{personTimo, "Alex: 8 questions in the plan", "8 questions in the plan", true},
		{"alex", "ALEX:merge it", "merge it", true},
		{personTimo, "Supervisor: the lease", "", false},
		{personTimo, ciWait, "", false},
		{personTimo, "", "", false},
		{"", ciWait, ciWait, true},
	} {
		if ask, ok := waitsOn(c.person, c.waits); ask != c.ask || ok != c.ok {
			t.Errorf("waitsOn(%q, %q) = %q %v, want %q %v", c.person, c.waits, ask, ok, c.ask, c.ok)
		}
	}
}

// stoppedWaits are three sessions waiting on Alex: one runs, one stopped
// 30m ago, one stopped 3h ago, and one whose wait is 3h old and whose end
// no watch saw.
func stoppedWaits() (*state.State, []*claude.Session, *app) {
	ask := personTimo + ": " + approveADR
	st := &state.State{Records: []state.Record{
		{Session: state.Party{Session: "sR", Name: "Runs"}, Waits: ask, At: relayNow.Add(-5 * time.Hour)},
		{Session: state.Party{Session: "sN", Name: "Stopped lately"}, Waits: ask, At: relayNow.Add(-time.Hour), Ended: relayNow.Add(-30 * time.Minute)},
		{Session: state.Party{Session: "sO", Name: "Stopped long ago"}, Waits: ask, At: relayNow.Add(-4 * time.Hour), Ended: relayNow.Add(-3 * time.Hour)},
		{Session: state.Party{Session: "sU", Name: "Ended unseen"}, Waits: ask, At: relayNow.Add(-3 * time.Hour)},
	}}
	sessions := []*claude.Session{{ID: "sR", Name: "Runs"}}
	var out strings.Builder
	a := &app{out: &out, now: relayNow, cfg: &config.Config{Guide: config.Guide{Person: personTimo, WaitingTTL: config.Duration{Duration: 2 * time.Hour}}}}
	return st, sessions, a
}

func TestGuideQueueFoldsSessionsStoppedPastTheTTL(t *testing.T) {
	st, sessions, a := stoppedWaits()
	q, stopped := a.guideQueue(st, sessions)
	a.printQueue(q)
	_, _ = fmt.Fprintln(a.out, a.stoppedLine(stopped))
	want := `"Runs" waits on its person: approve the ADR
"Stopped lately" (stopped) waits on its person: approve the ADR
2 stopped session(s) waited on Alex, stopped over 2h ago: beekeeper guide queue --full lists them
`
	if got := a.out.(*strings.Builder).String(); got != want {
		t.Fatalf("queue:\n%s\nwant:\n%s", got, want)
	}
	if len(stopped) != 2 || stopped[0].Owner != "Stopped long ago" || stopped[1].Owner != "Ended unseen" {
		t.Fatalf("folded: %+v", stopped)
	}
	a.cfg.Guide.WaitingTTL.Duration = 0
	if q, stopped := a.guideQueue(st, sessions); len(q) != 4 || stopped != nil {
		t.Fatalf("guide.waitingTTL 0 folds: %d kept, %d folded", len(q), len(stopped))
	}
}

func TestGuideFeedSaysNothingForSessionsThatAgedOut(t *testing.T) {
	st, sessions, a := stoppedWaits()
	lines, _ := a.feedLines(st, sessions, nil, nil)
	if !slices.Equal(lines, []string{
		`GUIDE WAITING: "Runs" needs its person: approve the ADR`,
		`GUIDE WAITING: "Stopped lately" (stopped) needs its person: approve the ADR`,
	}) {
		t.Fatalf("first poll: %q", lines)
	}
	a.now = relayNow.Add(2 * time.Hour) // "Stopped lately" ages out
	lines, changed := a.feedLines(st, sessions, nil, nil)
	if lines != nil || !changed {
		t.Fatalf("an aged-out session is news: %q (changed %v)", lines, changed)
	}
	if fed := st.GuideRole().Fed; len(fed) != 1 || !strings.Contains(fed[0], "sR") {
		t.Fatalf("fed: %q", fed)
	}
}

func TestGuideQueueSkipsAgentsThatReportedAfterTheWait(t *testing.T) {
	ask := personTimo + ": " + approveADR
	done := state.Party{Session: "sD", Name: "Finished"}
	idle := state.Party{Session: "sI", Name: "Reported"}
	busy := state.Party{Session: "sB", Name: "Asks again"}
	st := &state.State{
		Records: []state.Record{
			{Session: done, Waits: ask, At: relayNow.Add(-time.Hour)},
			{Session: idle, Waits: ask, At: relayNow.Add(-time.Hour)},
			{Session: busy, Waits: ask, At: relayNow.Add(-time.Hour)},
		},
		Agents: []state.Agent{
			{Party: done, Done: true, IdleSince: relayNow.Add(-30 * time.Minute)},
			{Party: idle, IdleSince: relayNow.Add(-30 * time.Minute)},
			{Party: busy, Task: "the next task", IdleSince: relayNow.Add(-2 * time.Hour)},
		},
	}
	sessions := []*claude.Session{{ID: "sD", Name: done.Name}, {ID: "sI", Name: idle.Name}, {ID: "sB", Name: busy.Name}}
	q, _ := guideQueue(st, sessions, personTimo, time.Time{})
	if len(q) != 1 || q[0].Owner != busy.Name {
		t.Fatalf("queue: %+v", q)
	}
	removeAgent(st, slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(busy) }))
	if q, _ := guideQueue(st, sessions, personTimo, time.Time{}); len(q) != 0 {
		t.Fatalf("an agent off the roster waits: %+v", q)
	}
}

const guideNoteReader = "Reader"

func TestGuideCentralFeedLifecycle(t *testing.T) {
	for _, verb := range []string{answerRecorded, noteDone, noteDefaulted, noteOvertaken} {
		t.Run(verb, func(t *testing.T) {
			a := &app{cfg: &config.Config{}}
			a.cfg.Guide.Person = guideNoteReader
			a.cfg.Central.Context = centralKey
			st := &state.State{Notes: []state.Note{{ID: 1, For: guideNoteReader, Text: "local?"}}}
			notes := []state.Note{{ID: 1, Central: centralKey, For: guideNoteReader, Text: "central?"}, {ID: 2, Central: centralKey, For: "Someone else", Text: "other?"}}
			lines, changed := a.centralGuideLines(st, nil, notes, nil, true)
			if !changed || len(lines) != 1 || !strings.Contains(lines[0], "GUIDE DECISION: central#1") {
				t.Fatalf("first poll: %q, %v", lines, changed)
			}
			a.feedLines(st, nil, nil, nil) // local delivery must retain the central mark
			if lines, changed := a.centralGuideLines(st, nil, notes, nil, true); len(lines) != 0 || changed {
				t.Fatalf("repeat: %q", lines)
			}
			if lines, changed := a.centralGuideLines(st, nil, nil, nil, false); len(lines) != 0 || changed {
				t.Fatalf("outage: %q", lines)
			}
			if lines, changed := a.centralGuideLines(st, nil, notes, nil, true); len(lines) != 0 || changed {
				t.Fatalf("recovery: %q", lines)
			}
			closed := map[int]state.Event{1: {Verb: verb, Detail: verbAnswer, By: state.Party{Name: mailGuide}}}
			lines, changed = a.centralGuideLines(st, nil, nil, closed, true)
			want := "GUIDE CLOSED"
			if verb == answerRecorded {
				want = "GUIDE ANSWERED"
			}
			if verb == noteDefaulted {
				want = "GUIDE DEFAULTED"
			}
			if !changed || len(lines) != 1 || !strings.HasPrefix(lines[0], want) || !strings.Contains(lines[0], "central#1") {
				t.Fatalf("closure: %q", lines)
			}
			if lines, changed := a.centralGuideLines(st, nil, nil, closed, true); len(lines) != 0 || changed {
				t.Fatalf("closed twice: %q", lines)
			}
		})
	}
}

func TestGuideHandoverAndStartReportAllProviderSubscriptions(t *testing.T) {
	a, _ := stubApp(t)
	origMachine := plat.Machine
	plat.Machine = tableMachine{plat.Machine}
	t.Cleanup(func() { plat.Machine = origMachine })

	t0 := a.now
	reset5h := t0.Add(3 * time.Hour).UTC()
	a.cfg.Capacity = config.Capacity{
		Ceiling: 6,
		Providers: map[string]config.Provider{
			claudeComm:          {Ceiling: 2, Usage: config.Usage{Command: []string{testReadClaude}}},
			capacity.HarnessAgy: {Ceiling: 2, Usage: config.Usage{Command: []string{testReadAgy}}},
			codex.Harness:       {Ceiling: 2, Usage: config.Usage{Command: []string{testReadCodex}}},
		},
		Choices: []config.Choice{
			{Harness: claudeComm, Model: pickOpus},
			{Harness: capacity.HarnessAgy, Model: pickGemini},
			{Harness: codex.Harness, Model: pickGPT},
		},
	}
	a.readUsage = func(_ context.Context, cmd []string, _ time.Duration) (capacity.UsageReading, error) {
		switch cmd[0] {
		case testReadClaude:
			return capacity.UsageReading{
				FiveHour: &capacity.UsageWindow{UsedPercent: 10, ResetsAt: reset5h},
				SevenDay: &capacity.UsageWindow{UsedPercent: 9, ResetsAt: t0.Add(150 * time.Hour).UTC()},
			}, nil
		case testReadAgy:
			return capacity.UsageReading{
				Buckets: map[string]capacity.UsageBucket{
					config.DefaultAgyGeminiBucket: {
						FiveHour: &capacity.UsageWindow{UsedPercent: 10, ResetsAt: reset5h},
						SevenDay: &capacity.UsageWindow{UsedPercent: 10, ResetsAt: t0.Add(112 * time.Hour).UTC()},
					},
				},
				BucketOrder: []string{config.DefaultAgyGeminiBucket},
			}, nil
		default:
			return capacity.UsageReading{
				FiveHour: &capacity.UsageWindow{UsedPercent: 10, ResetsAt: reset5h},
				SevenDay: &capacity.UsageWindow{UsedPercent: 50, ResetsAt: t0.Add(48 * time.Hour).UTC()},
			}, nil
		}
	}

	wantSubstrings := []string{
		"claude: ",
		"7d 9%",
		"pace 10.7%",
		"agy: ",
		"Gemini Models 5h 10%",
		"7d 10%",
		"pace 33.3%",
		"UNDERUSED (Gemini Models: 7d 10% is 23.3% behind pace 33.3% (below 50% of pace",
		"codex: ",
		"7d 50%",
		"pace 71.4%",
		"choice: agy / gemini-x (provider agy, bucket Gemini Models)",
	}

	prompt, err := runIn(a, a.guideHandoverCmd(), "--prompt")
	if err != nil {
		t.Fatalf("guide handover --prompt: %v", err)
	}
	if !strings.Contains(prompt, "### Subscription budgets\n") {
		t.Fatalf("prompt missing Subscription budgets section:\n%s", prompt)
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(prompt, want) {
			t.Errorf("guide handover --prompt missing %q:\n%s", want, prompt)
		}
	}

	full, err := runIn(a, a.guideHandoverCmd(), "--full")
	if err != nil {
		t.Fatalf("guide handover --full: %v", err)
	}
	if !strings.Contains(full, "## Subscription budgets\n") {
		t.Fatalf("handover missing Subscription budgets section:\n%s", full)
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(full, want) {
			t.Errorf("guide handover --full missing %q:\n%s", want, full)
		}
	}

	runLive(t, a, map[int]string{1: "Guide run 1"})
	a.as = ""
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s1")
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_NAME", "Guide run 1")
	startOut, err := runIn(a, a.guideCmd(), "start")
	if err != nil {
		t.Fatalf("guide start: %v", err)
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(startOut, want) {
			t.Errorf("guide start missing %q:\n%s", want, startOut)
		}
	}
}
