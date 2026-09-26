package cmd

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestOneSessionHoldsOneRole(t *testing.T) {
	st := handOverState() // supA supervises
	if _, _, err := guideRole.start(st, supA, false, false, relayNow); Code(err) != ExitRefused {
		t.Fatalf("the supervisor's session took the guide role: %v", err)
	}
	msg, evs, err := guideRole.start(st, agentC, false, false, relayNow)
	if err != nil || msg != `"Agent three" guides now` || evs[0].Verb != "guide.start" {
		t.Fatalf("guide start: %q %v %v", msg, evs, err)
	}
	if _, _, err := supervisorRole.start(st, agentC, true, true, relayNow); Code(err) != ExitRefused {
		t.Fatalf("the guide's session took the supervisor role: %v", err)
	}
	if _, _, err := guideRole.relay(st, agentC, supA, relayNow, 15*time.Minute); Code(err) != ExitRefused {
		t.Fatalf("the guide relayed its role to the supervisor: %v", err)
	}
	if !st.Supervisor.Is(supA) || !st.Guide.Holder.Is(agentC) {
		t.Fatalf("both roles run in two sessions: supervisor %+v, guide %+v", st.Supervisor, st.Guide.Holder)
	}
}

func TestGuideRelayLeavesTheSupervisorUntouched(t *testing.T) {
	st := handOverState()
	if _, _, err := guideRole.start(st, agentC, false, false, relayNow); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(st.SupervisorRole())
	grants := len(st.Grants)
	msg, _, err := guideRole.relay(st, agentC, agentD, relayNow, 15*time.Minute)
	if err != nil || strings.Contains(msg, "grants") || !strings.Contains(msg, "`beekeeper guide start`") {
		t.Fatalf("guide relay: %q %v", msg, err)
	}
	msg, _, err = guideRole.start(st, agentD, true, false, relayNow.Add(time.Minute))
	if err != nil || !strings.Contains(msg, `relieving "Agent three"`) || strings.Contains(msg, "grant") {
		t.Fatalf("successor start: %q %v", msg, err)
	}
	if relievedIn(st.GuideRole(), agentC) == nil || relievedBy(st, agentC) != nil {
		t.Fatal("the guide relay relieved the guide and only in the guide's record")
	}
	after, _ := json.Marshal(st.SupervisorRole())
	if string(before) != string(after) || len(st.Grants) != grants || !st.Grants[0].By.Is(supA) {
		t.Fatalf("the guide relay touched the supervisor:\n%s\n%s", before, after)
	}
	lines, _ := guideRole.fireRelay(st, relayNow.Add(time.Minute))
	if len(lines) != 1 || !strings.HasPrefix(lines[0], `GUIDE RELAY TAKEN: "Agent five" guides since`) {
		t.Fatalf("relay taken: %q", lines)
	}
}

func TestGuideRelayDueFiresOncePerTerm(t *testing.T) {
	st := &state.State{}
	guideRole.set(st, state.Role{Holder: &state.Supervisor{Party: agentC, Since: relayNow}})
	lines, evs := guideRole.fireRelayDue(st, quietness{checked: true, context: 50_000}, relayNow)
	if len(lines) != 1 || lines[0] != `GUIDE RELAY DUE: "Agent three" is at 50k tokens of context: beekeeper guide handover --prompt` || evs[0].Verb != "guide.relay-due" {
		t.Fatalf("relay due: %q %v", lines, evs)
	}
	if st.RelayDue != nil {
		t.Fatal("the guide's relay due landed in the supervisor's record")
	}
	if lines, _ := guideRole.fireRelayDue(st, quietness{checked: true, context: 60_000}, relayNow); lines != nil {
		t.Fatalf("relay due said twice: %q", lines)
	}
}

func TestGuideFeedSaysEachItemOnce(t *testing.T) {
	owner := state.Party{Session: "sO", Name: "Agent seven"}
	a := &app{now: relayNow, cfg: &config.Config{}} // guide.person unset: every --for note
	st := &state.State{Notes: []state.Note{
		{ID: 1, For: "Timo", Text: "merge the bump?", Default: "it waits", By: owner},
		{ID: 2, Text: "check the rollout", By: owner},
	}}
	agent := state.Party{Session: "sC", Name: agentC.Name}
	st.Records = []state.Record{{Session: agent, Issue: "o/r#1", Waits: personTimo + ": " + approveADR}}
	sessions := []*claude.Session{{ID: "sC", Name: agentC.Name}, {ID: "sO", Name: "Agent seven"}}
	lines, changed := a.feedLines(st, sessions, nil)
	if !changed || len(lines) != 2 ||
		lines[0] != `GUIDE DECISION: #1 for Timo from "Agent seven": merge the bump?; if unanswered: it waits` ||
		lines[1] != `GUIDE WAITING: "Agent three" needs its person: Timo: approve the ADR` {
		t.Fatalf("first poll: %q", lines)
	}
	if lines, changed := a.feedLines(st, sessions, nil); changed || lines != nil {
		t.Fatalf("second poll said again: %q", lines)
	}
	st.Notes = st.Notes[1:]
	st.Records[0].Waits = "Timo: pick a name"
	closed := map[int]state.Event{1: {Verb: "note.answered", By: state.Party{Name: "Guide"}, Detail: "#1 answered for Timo: yes, merge it (asked by Agent seven: merge the bump?)"}}
	lines, _ = a.feedLines(st, sessions, closed)
	if len(lines) != 2 || lines[0] != `GUIDE WAITING: "Agent three" needs its person: Timo: pick a name` ||
		lines[1] != "GUIDE ANSWERED (Guide): #1 answered for Timo: yes, merge it (asked by Agent seven: merge the bump?)" {
		t.Fatalf("answer and new wait: %q", lines)
	}
}

const (
	approveADR = "approve the ADR"
	personTimo = "Timo"
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
	a.printQueue(guideQueue(st, sessions, "Pat"))
	want := `#84 for Pat from "Agent three": refine the proposal: the split or one issue?
#99 for Pat from "Agent three": merge the context bump?; if unanswered: it waits
#100 for pat from "Agent one": relay due: pick the successor
"Agent three" waits on its person: approve the ADR
`
	if out.String() != want {
		t.Fatalf("queue for Pat:\n%s\nwant:\n%s", out.String(), want)
	}
	var ids []int
	for _, it := range guideQueue(st, sessions, "") {
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
	lines, changed := a.feedLines(st, sessions, nil)
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
	if lines, changed := a.feedLines(st, sessions, nil); changed || lines != nil {
		t.Fatalf("second poll said again: %q", lines)
	}
	st.Notes = slices.DeleteFunc(st.Notes, func(n state.Note) bool { return n.ID == 23 || n.ID == 99 })
	closed := map[int]state.Event{99: {Verb: "note.answered", By: state.Party{Name: "Guide"}, Detail: "#99 answered for Pat: yes"}}
	if lines, _ := a.feedLines(st, sessions, closed); !slices.Equal(lines, []string{"GUIDE ANSWERED (Guide): #99 answered for Pat: yes"}) {
		t.Fatalf("the person's answer, and nothing for the supervisor's closed note: %q", lines)
	}
}

const guideHost = "local_g"

func TestGuideQueueSkipsArchivedTestAndOwnSessions(t *testing.T) {
	guide := state.Party{Session: "sG", HostSession: guideHost, Name: "Guide Timo through his decisions"}
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
		{Session: party("sS", "local_s", "Land the follow-ups"), Waits: "Timo: merge it"},
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
	a.printQueue(guideQueue(st, sessions, personTimo))
	want := `"Agent three" waits on its person: approve the ADR
"Land the follow-ups" (stopped) waits on its person: merge it
`
	if out.String() != want {
		t.Fatalf("queue:\n%s\nwant:\n%s", out.String(), want)
	}
	a.cfg.Guide.Person = personTimo
	lines, _ := a.feedLines(st, sessions, nil)
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
	st := &state.State{Records: []state.Record{{Session: sup, Issue: "o/r#1"}}}
	sessions := []*claude.Session{{ID: "sP", Name: sup.Name, Waiting: &claude.Waiting{Turn: "t1", Action: "clarify: is klaus-lab-67 an agent"}}}
	if q := guideQueue(st, sessions, personTimo); len(q) != 0 {
		t.Fatalf("a report read as an ask is queued: %+v", q)
	}
	a := &app{now: relayNow, cfg: &config.Config{Guide: config.Guide{Person: personTimo}}}
	if lines, _ := a.feedLines(st, sessions, nil); lines != nil {
		t.Fatalf("a report read as an ask is fed: %q", lines)
	}
}

func TestWaitsOnReadsTheAskOfThePerson(t *testing.T) {
	for _, c := range []struct {
		person, waits, ask string
		ok                 bool
	}{
		{personTimo, "Timo: 8 questions in the plan", "8 questions in the plan", true},
		{"timo", "TIMO:merge it", "merge it", true},
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
