package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The decisions test's team channel, its question and the note_add keys.
const (
	teamChannel  = "C0123"
	whichLane    = "Which lane?"
	keyStatusQuo = "status_quo"
	keyDefault   = "default"
	keyWhy       = "why"
	ownLane      = "muster gets its own lane"
)

// fakeGateway is klaus-gateway's decisions surface: it keeps what it was
// sent and refuses a decision to nobody, as the real one does.
type fakeGateway struct {
	mu      sync.Mutex
	posted  []decision
	closed  []string
	tokens  []string
	refuses bool
}

func (g *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tokens = append(g.tokens, r.Header.Get("Authorization"))
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.URL.Path == "/decisions" && g.refuses:
		http.Error(w, "person: no Slack user has this email", http.StatusUnprocessableEntity)
	case r.URL.Path == "/decisions":
		var d decision
		dec := json.NewDecoder(strings.NewReader(string(body)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&d); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		g.posted = append(g.posted, d)
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"id":"d%d","channel":"D1","ts":"1.2"}`, len(g.posted))
	case strings.HasPrefix(r.URL.Path, "/decisions/") && strings.HasSuffix(r.URL.Path, "/close"):
		var c struct{ Outcome, Text string }
		_ = json.Unmarshal(body, &c)
		g.closed = append(g.closed, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/decisions/"), "/close")+" "+c.Outcome+": "+c.Text)
		_, _ = w.Write([]byte(`{"channel":"D1","ts":"1.2"}`))
	default:
		http.NotFound(w, r)
	}
}

func (g *fakeGateway) seen() ([]decision, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]decision{}, g.posted...), append([]string{}, g.closed...)
}

func TestEnvtestServeDecisions(t *testing.T) {
	e := newServeEnv(t)
	gw := &fakeGateway{}
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("sa-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.srv.cfg.Serve.People = map[string]string{"bo": boEmail}
	e.srv.cfg.Serve.Channels = map[string]string{ourTeam: teamChannel}
	e.srv.cfg.Serve.Gateway = config.Gateway{URL: srv.URL, TokenFile: tokenFile, AnswerTool: "x_beekeeper_note_answer"}
	e.srv.gw = newGateway(e.srv.cfg.Serve.Gateway)

	ana := e.as(t, e.token(t, "ana@example.com", teamGroup))
	bo := e.as(t, e.token(t, boEmail, "giantswarm:team-planeteers"))
	kim := e.as(t, e.token(t, "kim@example.com", teamGroup))
	ask := func(forWho, due string) int {
		args := map[string]any{paramText: "Which lane for muster?", paramFor: forWho, paramKind: noteDecision, paramAgent: anaAgent, paramHost: lab,
			keyStatusQuo: "muster has no lane", keyWhy: "only its owners pick its lane", "options": []any{"portal: it rolls with backstage", "own: a lane of its own"}, "recommend": 2,
			dueID: due, keyDefault: ownLane}
		id, _ := e.expect(t, ana, "note_add", args, false, "note #")["id"].(float64)
		return int(id)
	}

	// A person's decision: posted as a direct message, answered by a click.
	n := ask("bo", "3h")
	posted, _ := gw.seen()
	if len(posted) != 1 {
		t.Fatalf("posted %+v", posted)
	}
	d := posted[0]
	if d.Person != boEmail || d.Team != "" || d.Question != "Which lane for muster?" || d.StatusQuo != "muster has no lane" ||
		len(d.Options) != 2 || d.Options[1].Label != "own" || d.Options[1].Consequence != "a lane of its own" || d.Recommend != 2 ||
		d.Default != ownLane || d.Note != "note #"+strconv.Itoa(n) || d.AskedBy != "ana@example.com/ana-agent on lab" ||
		d.Answer.Tool != "x_beekeeper_note_answer" || d.Answer.Arguments[paramVia] != viaSlack {
		t.Fatalf("decision %+v", d)
	}
	if gw.tokens[0] != "Bearer sa-token" {
		t.Fatalf("token %q", gw.tokens[0])
	}
	e.expect(t, kim, "note_answer", map[string]any{paramNote: n, paramChoice: 1}, true, "only its addressee")
	e.expect(t, bo, "note_answer", map[string]any{paramNote: n, paramChoice: 2, paramText: "until Friday", paramVia: viaSlack}, false, "answered and closed")
	if _, closed := gw.seen(); len(closed) != 1 || closed[0] != "d1 answered: own — until Friday" {
		t.Fatalf("closed %q", closed)
	}
	evs, err := e.store.Events(0, func(ev state.Event) bool { return ev.Verb == noteAnswered })
	if err != nil || len(evs) != 1 || evs[0].By.Person != boEmail || !strings.Contains(evs[0].Detail, "via slack: own — until Friday") {
		t.Fatalf("note.answered %+v %v", evs, err)
	}

	// A team's decision: posted to its channel, any member answers.
	n = ask("team:"+ourTeam, "3h")
	posted, _ = gw.seen()
	if d := posted[1]; d.Team != ourTeam || d.Channel != teamChannel || d.Person != "" {
		t.Fatalf("team decision %+v", d)
	}
	e.expect(t, bo, "note_answer", map[string]any{paramNote: n, paramText: portalLane}, true, "only its addressee")
	e.expect(t, kim, "note_answer", map[string]any{paramNote: n, paramText: "portal, after the release"}, false, "answered and closed")

	// Withdrawn by its filer; defaulted at its due time.
	n = ask("bo", "3h")
	e.expect(t, bo, "note_done", map[string]any{paramNote: n}, true, "only that person")
	e.expect(t, ana, "note_done", map[string]any{paramNote: n}, false, "done")
	n = ask("bo", "1h")
	if err := e.srv.defaultDue(context.Background(), time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	e.expect(t, bo, "note_answer", map[string]any{paramNote: n, paramChoice: 1}, true, "not open")
	if _, closed := gw.seen(); len(closed) != 4 || closed[2] != "d3 withdrawn: " || closed[3] != "d4 defaulted: "+ownLane {
		t.Fatalf("closed %q", closed)
	}
	if evs, _ := e.store.Events(0, func(ev state.Event) bool { return ev.Verb == noteDefaulted }); len(evs) != 1 {
		t.Fatalf("note.defaulted %+v", evs)
	}

	// A decision that renders not, or reaches nobody, is refused and not kept.
	if text, _, failed := ana.call("note_add", map[string]any{paramText: strings.Repeat("x", questionMax+1), paramFor: "bo", paramKind: noteDecision,
		keyStatusQuo: "s", keyWhy: "w", dueID: "3h", keyDefault: ownLane}); !failed || !strings.Contains(text, "cannot render") {
		t.Fatalf("long question: %v %s", failed, text)
	}
	if text, _, failed := ana.call("note_add", map[string]any{paramText: whichLane, paramFor: "eve", paramKind: noteDecision,
		keyStatusQuo: "s", keyWhy: "w", dueID: "3h", keyDefault: ownLane}); !failed || !strings.Contains(text, "nobody's name") {
		t.Fatalf("unknown person: %v %s", failed, text)
	}
	gw.mu.Lock()
	gw.refuses = true
	gw.mu.Unlock()
	if text, _, failed := ana.call("note_add", map[string]any{paramText: whichLane, paramFor: "bo", paramKind: noteDecision,
		keyStatusQuo: "s", keyWhy: "w", dueID: "3h", keyDefault: ownLane}); !failed || !strings.Contains(text, "not delivered, withdrawn") {
		t.Fatalf("refused by the gateway: %v %s", failed, text)
	}
	if st, _ := e.store.Read(); len(st.Notes) != 0 {
		t.Fatalf("open notes %+v", st.Notes)
	}
}
