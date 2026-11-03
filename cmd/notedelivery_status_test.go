package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/internal/central/centraltest"
	"github.com/giantswarm/beekeeper/internal/identity"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/state/kube"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

const noteDeliveryWords = "Use the fixture"

func TestNoteDeliveryKeepsAnswerOutcome(t *testing.T) {
	a, _ := noteApp(t)
	n := state.Note{ID: 1, By: state.Party{Session: offID, Name: agentOne}, Text: laneQuestion}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Notes = []state.Note{n}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	previous := wakeOwner
	t.Cleanup(func() { wakeOwner = previous })
	failure := errors.New("session unavailable")
	wakeOwner = func(*app, context.Context, state.Party, string, string, string) error { return failure }
	line := answeredLine(n.ID, notePerson, noteDeliveryWords)
	if got := a.tellFiler(t.Context(), state.Party{}, &n, line); !strings.Contains(got, failure.Error()) {
		t.Fatalf("failure not reported: %q", got)
	}
	check := func(want string) {
		t.Helper()
		shown, err := a.localNote(n.ID)
		if err != nil || shown == nil || shown.Delivery == nil || shown.Delivery.Status != want {
			t.Fatalf("notification: %+v, %v, want %s", shown, err, want)
		}
	}
	check("failed")
	failure = nil
	a.tellFiler(t.Context(), state.Party{}, &n, "HOLD RELEASED")
	check("failed") // a later hold message cannot hide a failed answer
	a.tellFiler(t.Context(), state.Party{}, &n, line)
	check("delivered") // an actual successful retry supersedes the failure
}

func TestNoteDeliveryMailboxFailure(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	kstore := kube.New(fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Note{}).Build())
	a, _ := noteApp(t)
	a.central = true
	store := &auditStore{Store: kstore}
	a.store = store
	n := state.Note{ID: 1, Kind: noteDecision, For: boEmail, Text: laneQuestion, Refs: []string{decisionRef}, By: state.Party{Name: agentOne, Host: "test-machine", Team: ourTeam}}
	n2 := state.Note{ID: 2, Kind: noteDecision, For: boEmail, Text: laneQuestion, Refs: []string{decisionRef}, By: state.Party{Name: agentOne, Host: "test-machine", Team: ourTeam}}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Notes = []state.Note{n, n2}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	a.mailFiler = func(context.Context, *state.Note, string) error { return errors.New("mailbox unavailable") }
	srv := &server{cfg: a.cfg, store: kstore}
	c := &call{ctx: t.Context(), s: srv, who: identity.Caller{Email: boEmail, Team: ourTeam}, app: a, store: store, out: &bytes.Buffer{}}
	a.out = c.out
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{paramNote: 1, paramText: noteDeliveryWords}
	res, err := toolNoteAnswer(c, req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.out.String(), "retaining notification status failed") || !strings.Contains(c.out.String(), "notification failed (mailbox unavailable)") {
		t.Fatalf("central note_answer output: %q", c.out.String())
	}
	m, _ := res.(map[string]any)
	d, _ := m["delivery"].(*noteDelivery)
	if d == nil || d.Status != noteDeliveryFailed || !strings.Contains(d.Detail, "mailbox unavailable") {
		t.Fatalf("toolNoteAnswer delivery: %+v", res)
	}
	shown, err := a.localNote(1)
	if err != nil || shown == nil || shown.Delivery == nil || shown.Delivery.Status != noteDeliveryFailed || !strings.Contains(shown.Delivery.Detail, "mailbox unavailable") {
		t.Fatalf("retained central delivery: %+v, %v", shown, err)
	}
	a.mailFiler = func(context.Context, *state.Note, string) error { return nil }
	c2 := &call{ctx: t.Context(), s: srv, who: identity.Caller{Email: boEmail, Team: ourTeam}, app: a, store: store, out: &bytes.Buffer{}}
	a.out = c2.out
	req2 := mcp.CallToolRequest{}
	req2.Params.Arguments = map[string]any{paramNote: 2, paramText: noteDeliveryWords}
	res2, err := toolNoteAnswer(c2, req2)
	if err != nil || strings.Contains(c2.out.String(), "retaining notification status failed") {
		t.Fatalf("central queued note_answer: %v, %q", err, c2.out.String())
	}
	if m2, _ := res2.(map[string]any); m2["delivery"] == nil || m2["delivery"].(*noteDelivery).Status != "queued" {
		t.Fatalf("toolNoteAnswer queued delivery: %+v", res2)
	}
	shown2, err := a.localNote(2)
	if err != nil || shown2 == nil || shown2.Delivery == nil || shown2.Delivery.Status != "queued" {
		t.Fatalf("retained queued central delivery: %+v, %v", shown2, err)
	}
	feedEvs, err := kstore.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, fe := range feedEvs {
		if fe.Kind == noteDeliveryEvent {
			t.Fatalf("feed leaked internal delivery event: %+v", fe)
		}
	}

	// answerCentral surfaces the central notification failure and delivery status.
	mcpSrv := mcpserver.NewMCPServer(mailServer, "test")
	mcpSrv.AddTool(mcp.NewTool("note_answer"), func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultStructured(res, strings.TrimSpace(c.out.String())), nil
	})
	h := httptest.NewServer(mcpserver.NewStreamableHTTPServer(mcpSrv, mcpserver.WithStateLess(true)))
	t.Cleanup(h.Close)
	clientApp, clientOut, _ := centralNoteApp(t, &noteAddStandIn{})
	cm := centraltest.New(t, h.URL+"/mcp", mailServer)
	bin, _ := centraltest.Binary(t, cm.URL, "token")
	clientApp.cfg.Central.Muster = bin
	if err := clientApp.answerCentral(t.Context(), 1, 0, viaCLI, noteDeliveryWords, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(clientOut.String(), "notification failed (mailbox unavailable)") {
		t.Fatalf("answerCentral did not surface notification failure: %q", clientOut.String())
	}
	clientOut.Reset()
	clientApp.json = true
	if err := clientApp.answerCentral(t.Context(), 1, 0, viaCLI, noteDeliveryWords, false); err != nil {
		t.Fatal(err)
	}
	var gotJSON struct {
		Note     int           `json:"note"`
		Delivery *noteDelivery `json:"delivery"`
	}
	if err := json.Unmarshal([]byte(clientOut.String()), &gotJSON); err != nil || gotJSON.Note != 1 || gotJSON.Delivery == nil || gotJSON.Delivery.Status != noteDeliveryFailed {
		t.Fatalf("answerCentral --json: %v, %s", err, clientOut.String())
	}
}
