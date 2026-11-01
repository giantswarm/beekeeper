package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/giantswarm/beekeeper/internal/central/centraltest"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/state/kube"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
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
	ana := newPerson(t, m, anaToken, anaEmail, ourTeam, anaLaptop, anaAgent)
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
	// bo's CLI calls as his agent: the central instance cannot tell his
	// terminal from it, so his answer reaches ana as relayed.
	msgs, _ := e.expect(t, e.as(t, anaToken), "receive_messages", map[string]any{}, false, "")["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("ana's mailbox %+v, want the answer", msgs)
	}
	raw, _ := json.Marshal(msgs[0].(map[string]any)["envelope"])
	var env mailEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || !strings.HasPrefix(env.Message.text(), fmt.Sprintf("NOTE ANSWERED #%d by %s (relayed by bo-agent): own", lane, boEmail)) {
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

// An answer to a central note is recorded whole up to answerMax characters,
// though its answer.recorded Event, the note's question with it, exceeds the
// API server's 1024 bytes for a message: bo answers ana's decisions with 470
// and 1000 characters, ana's agent is told each word for word, and one
// character over answerMax is refused with nothing recorded.
func TestEnvtestCentralAnswerLong(t *testing.T) {
	e := newServeEnv(t)
	decisionsVia(t, e, &fakeGateway{})
	stubAnswerGH(t, &answerGH{})
	m := centraltest.New(t, e.url, "beekeeper")
	anaToken := e.token(t, anaEmail, teamGroup)
	ana := newPerson(t, m, anaToken, anaEmail, ourTeam, anaLaptop, anaAgent)
	bo := newPerson(t, m, e.token(t, boEmail, "giantswarm:team-planeteers"), boEmail, piaTeam, "bo-laptop", "bo-agent")

	for _, size := range []int{470, 1000} {
		id := ana.fileCentral(fmt.Sprintf("Which lane for muster, answered in %d characters?", size), "3h")
		words := "keep the lane, " + strings.Repeat("ä", size-len("keep the lane, "))
		if out, code := bo.run("note", "answer", fmt.Sprintf("lab#%d", id), words); code != 0 || !strings.Contains(out, words) {
			t.Fatalf("bo's answer of %d characters: exit %d: %s", size, code, out)
		}
		evs, err := e.store.Events(0, func(ev state.Event) bool {
			return ev.Verb == answerRecorded && strings.Contains(ev.Detail, fmt.Sprintf("note #%d ", id))
		})
		if err != nil || len(evs) != 1 || !strings.HasSuffix(evs[0].Detail, ": "+words) || len(evs[0].Detail) <= 1024 && size == 1000 {
			t.Fatalf("answer.recorded of %d characters: %v %+v", size, err, evs)
		}
		msgs, _ := e.expect(t, e.as(t, anaToken), "receive_messages", map[string]any{}, false, "")["messages"].([]any)
		if len(msgs) != 1 {
			t.Fatalf("ana's mailbox %+v, want the answer of %d characters", msgs, size)
		}
		raw, _ := json.Marshal(msgs[0].(map[string]any)["envelope"])
		var env mailEnvelope
		if err := json.Unmarshal(raw, &env); err != nil || !strings.Contains(env.Message.text(), words) {
			t.Fatalf("ana's delivery of %d characters %+v: %v", size, env, err)
		}
		e.expect(t, e.as(t, anaToken), "ack_messages", map[string]any{paramIDs: []any{msgs[0].(map[string]any)["id"]}}, false, "")
	}

	id := ana.fileCentral("Which lane for muster, answered at length?", "3h")
	if out, code := bo.run("note", "answer", fmt.Sprintf("lab#%d", id), strings.Repeat("ä", answerMax+1)); code != ExitUsage ||
		!strings.Contains(out, fmt.Sprintf("at most %d", answerMax)) {
		t.Fatalf("bo's answer over %d characters: exit %d: %s", answerMax, code, out)
	}
}

// A worker parks on a central note and folds into one: ana's agent parks
// on her decision for bo by lab#<n> and by its bare number, a closed or
// unknown central note is refused, a second decision folds into the first
// by lab#<n> and by its bare number, and once bo answers, ana's watch
// settles the park with bo's answer word for word, read from the feed.
func TestEnvtestCentralParkAndFold(t *testing.T) {
	e := newServeEnv(t)
	decisionsVia(t, e, &fakeGateway{})
	stubAnswerGH(t, &answerGH{})
	m := centraltest.New(t, e.url, "beekeeper")
	ana := newPerson(t, m, e.token(t, anaEmail, teamGroup), anaEmail, ourTeam, anaLaptop, anaAgent)
	bo := newPerson(t, m, e.token(t, boEmail, "giantswarm:team-planeteers"), boEmail, piaTeam, "bo-laptop", "bo-agent")
	local, err := state.Open(ana.state)
	if err != nil {
		t.Fatal(err)
	}
	err = local.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Name: anaAgent}, Task: "pick muster's lane"}}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	lane := ana.fileCentral("Which lane for muster?", "3h")
	laneRef := fmt.Sprintf("lab#%d", lane)
	for _, on := range []string{laneRef, strconv.Itoa(lane)} {
		if out, code := ana.run("agents", "park", "--on", on, "bo's lane"); code != 0 || !strings.Contains(out, "parked on "+laneRef+": bo's lane") {
			t.Fatalf("ana's park --on %s: exit %d: %s", on, code, out)
		}
	}
	if out, code := ana.run("agents", "park", "--on", "lab#999", "nothing"); code != ExitRefused || !strings.Contains(out, "lab#999 is not open") {
		t.Fatalf("ana's park on a note not open: exit %d: %s", code, out)
	}

	fold := func(into, question string) {
		t.Helper()
		out, code := ana.run("note", "add", "--for", "bo", "--due", "3h", "--default", ownLane, "--status-quo", noMuseLane,
			"--why", "only its owners pick its lane", "--option", "portal: it rolls with backstage", "--option", "own: a lane of its own",
			"--recommend", "2", "--ref", privateThread, "--fold-into", into, question)
		if code != 0 || !strings.HasPrefix(out, "note "+laneRef+" (folded: --fold-into") {
			t.Fatalf("ana's fold into %s: exit %d: %s", into, code, out)
		}
	}
	fold(laneRef, "Which lane for the portal?")
	fold(strconv.Itoa(lane), "Which lane for agentlab?")
	out, code := bo.run("note", "list", "--full")
	if code != 0 || strings.Count(out, "lab#") != 1 || !strings.Contains(out, "Which lane for the portal?") || !strings.Contains(out, "Which lane for agentlab?") {
		t.Fatalf("bo's note list after the folds: exit %d: %s", code, out)
	}

	var watched bytes.Buffer
	w := ana.watcher(&watched)
	st, err := w.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if parks := w.settledParks(context.Background(), st); len(parks) != 0 {
		t.Fatalf("parks settled while the note is open: %+v", parks)
	}
	if out, code = bo.run("note", "answer", laneRef, "--choice", "2", "until Friday"); code != 0 {
		t.Fatalf("bo's answer: exit %d: %s", code, out)
	}
	parks := w.settledParks(context.Background(), st)
	if len(parks) != 1 || parks[0].on != laneRef || !strings.HasPrefix(parks[0].answer, "note "+laneRef+" answered by ") ||
		!strings.HasSuffix(parks[0].answer, ": own — until Friday") {
		t.Fatalf("parks settled after bo's answer: %+v (watch: %s)", parks, watched.String())
	}
}

// An answer before the first desk listing removes the note from that
// listing. An unfinished create, in contrast, stays open and defaults.
func TestEnvtestCentralNoteListingLifecycle(t *testing.T) {
	e := newServeEnv(t)
	decisionsVia(t, e, &fakeGateway{})
	stubAnswerGH(t, &answerGH{})
	m := centraltest.New(t, e.url, "beekeeper")
	ana := newPerson(t, m, e.token(t, anaEmail, teamGroup), anaEmail, ourTeam, anaLaptop, anaAgent)
	bo := newPerson(t, m, e.token(t, boEmail, "giantswarm:team-planeteers"), boEmail, piaTeam, "bo-laptop", "bo-agent")
	id := ana.fileCentral("Which lane before the first list?", "3h")
	ref := fmt.Sprintf("lab#%d", id)
	if out, code := bo.run("note", "answer", ref, "--choice", "2"); code != 0 {
		t.Fatalf("answer: %d %s", code, out)
	}
	if out, code := ana.run("note", "list"); code != 0 || !strings.Contains(out, "no open notes") {
		t.Fatalf("first listing after answer: %d %s", code, out)
	}
	if out, code := ana.run("note", "done", ref); code != ExitRefused {
		t.Fatalf("withdraw answered: %d %s", code, out)
	}
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ctrlclient.New(e.rc, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	filer := v1alpha1.Party{Name: anaAgent, Person: anaEmail, Team: ourTeam, Host: anaLaptop}
	orphan := &v1alpha1.Note{ObjectMeta: metav1.ObjectMeta{Namespace: "beekeeper-bumblebee", Name: "note-999"}, Spec: v1alpha1.NoteSpec{ID: 999, Filer: &filer, FiledAt: ptrTime(now), For: "bo", Kind: noteDecision, Text: "Keep the release unpublished?", Due: ptrTime(now.Add(time.Hour)), Default: ownLane}}
	if err := c.Create(context.Background(), orphan); err != nil {
		t.Fatal(err)
	}
	if out, code := bo.run("note", "list"); code != 0 || !strings.Contains(out, "lab#999") {
		t.Fatalf("unfinished create listing: %d %s", code, out)
	}
	if out, code := bo.run("guide", "queue", "--full"); code != 0 || !strings.Contains(out, "lab#999") {
		t.Fatalf("unfinished create queue: %d %s", code, out)
	}
	if err := e.srv.defaultDue(context.Background(), now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	st, err := e.store.Read()
	if err != nil || len(st.Defaulted) != 1 || st.Defaulted[0].ID != 999 || st.Defaulted[0].By.Person != anaEmail {
		t.Fatalf("defaulted state: %+v %v", st, err)
	}
}

func ptrTime(t time.Time) *metav1.Time { v := metav1.NewTime(t); return &v }
