package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/central/centraltest"
	"github.com/giantswarm/beekeeper/internal/state"
)

// fileCentral files a decision for bo from p's machine with note add, due
// after due, and returns its central number.
func (p *person) fileCentral(question, due string) int {
	p.t.Helper()
	out, code := p.run("note", "add", "--for", "bo", "--due", due, "--default", ownLane, "--status-quo", noMuseLane,
		"--why", "only its owners pick its lane", "--option", "portal: it rolls with backstage", "--option", "own: a lane of its own",
		"--recommend", "2", "--ref", privateThread, question)
	if code != 0 || !strings.Contains(out, "(central lab") {
		p.t.Fatalf("note add %q: exit %d: %s", question, code, out)
	}
	n, err := strconv.Atoi(strings.TrimPrefix(strings.Fields(out)[1], "#"))
	if err != nil {
		p.t.Fatalf("note add %q: %s", question, out)
	}
	return n
}

// The central instance's notes are listed, queued, answered and withdrawn
// on another person's machine beside its own: ana files decisions for bo
// on her machine, bo's machine lists and queues them as lab#<n>, bo
// answers one as himself, ana cannot answer it and bo cannot withdraw
// hers, ana withdraws one by its bare number, a number open on both is
// refused, and an unreachable central instance leaves the machine's own.
func TestEnvtestCentralNotesTwoPersons(t *testing.T) {
	e := newServeEnv(t)
	gw := &fakeGateway{}
	decisionsVia(t, e, gw)
	stubAnswerGH(t, &answerGH{})
	m := centraltest.New(t, e.url, "beekeeper")
	anaToken := e.token(t, anaEmail, teamGroup)
	ana := newPerson(t, m, anaToken, anaEmail, ourTeam, "ana-laptop", anaAgent)
	bo := newPerson(t, m, e.token(t, boEmail, "giantswarm:team-planeteers"), boEmail, piaTeam, "bo-laptop", "bo-agent")
	f, err := os.OpenFile(bo.cfg, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("guide: {person: bo}\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	lane := ana.fileCentral("Which lane for muster?", "3h")
	portal := ana.fileCentral("Does the portal roll first?", "2h")
	// bo's machine has a note of its own under lane's number, and bo's
	// agent is its guide.
	local, err := state.Open(bo.state)
	if err != nil {
		t.Fatal(err)
	}
	err = local.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide = &state.Role{Holder: &state.Supervisor{Party: state.Party{Name: "bo-agent"}}}
		st.Notes = []state.Note{{ID: lane, Kind: noteDecision, For: "bo", Text: "Keep the local lab?", By: state.Party{Name: "bo-agent"}, Refs: []string{privateThread}}}
		st.NextNote = lane
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	laneRef, portalRef := fmt.Sprintf("lab#%d", lane), fmt.Sprintf("lab#%d", portal)

	out, code := bo.run("note", "list")
	if code != 0 || !strings.Contains(out, fmt.Sprintf("#%d [for bo", lane)) || !strings.Contains(out, laneRef+" [for bo, by ") ||
		!strings.Contains(out, portalRef+" [for bo, by ") || !strings.Contains(out, "Which lane for muster?") {
		t.Fatalf("bo's note list: exit %d: %s", code, out)
	}
	out, code = bo.run("note", "list", "--json")
	var listed []state.Note
	if code != 0 || json.Unmarshal([]byte(out), &listed) != nil || len(listed) != 3 || listed[0].Central != "" || listed[1].Central != lab {
		t.Fatalf("bo's note list --json: exit %d: %s", code, out)
	}
	out, code = bo.run("guide", "queue")
	if code != 0 || !strings.Contains(out, fmt.Sprintf("#%d for bo from %q", lane, "bo-agent")) ||
		!strings.Contains(out, laneRef+" for bo from ") || !strings.Contains(out, " on ana-laptop") || !strings.Contains(out, portalRef+" for bo") {
		t.Fatalf("bo's guide queue: exit %d: %s", code, out)
	}
	// The central note due first is served first, then again until answered.
	for _, still := range []string{"asking ", "still asking "} {
		out, code = bo.run("guide", "next")
		if code != 0 || !strings.HasPrefix(out, still+portalRef+" for bo") || !strings.Contains(out, "beekeeper note answer "+portalRef) {
			t.Fatalf("bo's guide next: exit %d: %s", code, out)
		}
	}

	// A number open on both is refused, naming both; only bo answers his.
	if out, code = bo.run("note", "answer", strconv.Itoa(lane), "--choice", "2"); code != ExitRefused || !strings.Contains(out, laneRef) {
		t.Fatalf("bo's bare answer of #%d: exit %d: %s", lane, code, out)
	}
	if out, code = ana.run("note", "answer", laneRef, "--choice", "1"); code != ExitRefused || !strings.Contains(out, "only its addressee") {
		t.Fatalf("ana's answer of bo's note: exit %d: %s", code, out)
	}
	if out, code = bo.run("note", "answer", laneRef, "--choice", "2", "until Friday"); code != 0 ||
		!strings.Contains(out, "note "+laneRef+" answered as "+boEmail+": own") {
		t.Fatalf("bo's answer: exit %d: %s", code, out)
	}
	msgs, _ := e.expect(t, e.as(t, anaToken), "receive_messages", map[string]any{}, false, "")["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("ana's mailbox %+v, want the answer", msgs)
	}
	raw, _ := json.Marshal(msgs[0].(map[string]any)["envelope"])
	var env mailEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || !strings.HasPrefix(env.Message.text(), fmt.Sprintf("NOTE ANSWERED #%d by bo: own", lane)) {
		t.Fatalf("ana's delivery %+v: %v", env, err)
	}

	// The filer withdraws hers by its bare number; the addressee cannot.
	if out, code = bo.run("note", "done", portalRef); code != ExitRefused {
		t.Fatalf("bo's done of ana's note: exit %d: %s", code, out)
	}
	if out, code = bo.run("note", "done", portalRef, "--overtaken", "settled"); code != ExitUsage {
		t.Fatalf("bo's done --overtaken of a central note: exit %d: %s", code, out)
	}
	if out, code = ana.run("note", "done", strconv.Itoa(portal)); code != 0 || !strings.Contains(out, "note "+portalRef+" done") {
		t.Fatalf("ana's done: exit %d: %s", code, out)
	}
	if out, code = bo.run("note", "list"); code != 0 || strings.Contains(out, "lab#") || !strings.Contains(out, fmt.Sprintf("#%d [for bo", lane)) {
		t.Fatalf("bo's note list after both closed: exit %d: %s", code, out)
	}
	if out, code = bo.run("guide", "next"); code != 0 || !strings.HasPrefix(out, fmt.Sprintf("asking #%d for bo", lane)) {
		t.Fatalf("bo's guide next after the central ones: exit %d: %s", code, out)
	}

	// An unreachable central instance: the machine's notes stand.
	m.Down(true)
	if out, code = bo.run("note", "list"); code != ExitCentral || !strings.Contains(out, fmt.Sprintf("#%d [for bo", lane)) {
		t.Fatalf("bo's note list, central down: exit %d: %s", code, out)
	}
	if out, code = bo.run("guide", "queue", "--full"); code != 0 || !strings.Contains(out, "warning: ") || !strings.Contains(out, fmt.Sprintf("#%d for bo", lane)) {
		t.Fatalf("bo's guide queue, central down: exit %d: %s", code, out)
	}
	if out, code = bo.run("note", "done", strconv.Itoa(lane)); code != 0 {
		t.Fatalf("bo's done of his own note, central down: exit %d: %s", code, out)
	}
}
