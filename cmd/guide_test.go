package cmd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
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
	a := &app{now: relayNow}
	st := &state.State{Notes: []state.Note{
		{ID: 1, For: "Timo", Text: "merge the bump?", Default: "it waits", By: owner},
		{ID: 2, Text: "check the rollout", By: owner},
	}}
	waiting := &claude.Session{ID: "sC", Name: "Agent three", Waiting: &claude.Waiting{Turn: "t1", Action: "approve the ADR"}}
	sessions := []*claude.Session{waiting, {ID: "sO", Name: "Agent seven"}}
	lines, changed := a.feedLines(st, sessions, nil)
	if !changed || len(lines) != 2 ||
		lines[0] != `GUIDE DECISION: #1 for Timo from "Agent seven": merge the bump?; if unanswered: it waits` ||
		lines[1] != `GUIDE WAITING: "Agent three" needs its person: approve the ADR` {
		t.Fatalf("first poll: %q", lines)
	}
	if lines, changed := a.feedLines(st, sessions, nil); changed || lines != nil {
		t.Fatalf("second poll said again: %q", lines)
	}
	st.Notes = st.Notes[1:]
	waiting.Waiting = &claude.Waiting{Turn: "t2", Action: "pick a name"}
	closed := map[int]state.Event{1: {Verb: "note.answered", By: state.Party{Name: "Guide"}, Detail: "#1 answered for Timo: yes, merge it (asked by Agent seven: merge the bump?)"}}
	lines, _ = a.feedLines(st, sessions, closed)
	if len(lines) != 2 || lines[0] != `GUIDE WAITING: "Agent three" needs its person: pick a name` ||
		lines[1] != "GUIDE ANSWERED (Guide): #1 answered for Timo: yes, merge it (asked by Agent seven: merge the bump?)" {
		t.Fatalf("answer and new wait: %q", lines)
	}
}

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
