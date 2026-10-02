package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/beekeeper/internal/feed"
	"github.com/giantswarm/beekeeper/internal/mailbox"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/state/kube"
)

const (
	anaEmail = "ana@example.com"
	boEmail  = "bo@example.com"
	// anaAddress is ana's agent on the roster once registered on lab.
	anaAddress = "local:" + lab + "/" + anaAgent
	// keyRole is an A2A Message's role; purposeProof a test lease's purpose.
	keyRole      = "role"
	purposeProof = "proof"
)

// read reads a resource: its JSON, or the error.
func (m *mcpCaller) read(uri string) (map[string]any, error) {
	m.t.Helper()
	req := mcp.ReadResourceRequest{}
	req.Params.URI = uri
	res, err := m.c.ReadResource(context.Background(), req)
	if err != nil {
		return nil, err
	}
	if len(res.Contents) != 1 {
		m.t.Fatalf("%s: %d contents", uri, len(res.Contents))
	}
	tc, ok := res.Contents[0].(mcp.TextResourceContents)
	if !ok {
		m.t.Fatalf("%s: %T", uri, res.Contents[0])
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &v); err != nil {
		m.t.Fatalf("%s: %v: %s", uri, err, tc.Text)
	}
	return v, nil
}

// updates is a subscriptions/listen stream's notifications/resources/updated.
type updates struct {
	t    *testing.T
	uris chan string
}

// listen opens a stream as token for uris, once acknowledged; nil and the
// error when it is refused.
func (e *serveEnv) listen(t *testing.T, token string, uris ...string) (*updates, error) {
	t.Helper()
	m := e.as(t, token)
	u := &updates{t: t, uris: make(chan string, 1000)}
	acked := make(chan struct{}, 1)
	m.c.OnNotification(func(n mcp.JSONRPCNotification) {
		switch n.Method {
		case mcp.MethodNotificationResourceUpdated:
			if n.Params.Meta[mcp.MetaKeySubscriptionID] == nil {
				t.Errorf("an update without its subscription id: %v", n.Params)
			}
			u.uris <- fmt.Sprint(n.Params.AdditionalFields["uri"])
		case mcp.MethodNotificationSubscriptionsAcknowledged:
			acked <- struct{}{}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	failed := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := m.c.Listen(ctx, mcp.SubscriptionFilter{ResourceSubscriptions: uris}); err != nil && ctx.Err() == nil {
			failed <- err
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-acked:
		return u, nil
	case err := <-failed:
		return nil, err
	case <-time.After(10 * time.Second):
		t.Fatal("subscriptions/listen was neither acknowledged nor refused")
	}
	return nil, nil
}

// expect waits for an update of uri.
func (u *updates) expect(uri string) {
	u.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case got := <-u.uris:
			if got == uri {
				return
			}
		case <-deadline:
			u.t.Fatalf("no update of %s", uri)
		}
	}
}

// none checks no update of uri arrived; the caller waited for a later one
// first.
func (u *updates) none(uri string) {
	u.t.Helper()
	for {
		select {
		case got := <-u.uris:
			if got == uri {
				u.t.Fatalf("an update of %s, which did not change for this subscriber", uri)
			}
		default:
			return
		}
	}
}

func TestEnvtestServeResourcesReadableOnlyByWhoMay(t *testing.T) {
	e := newServeEnv(t)
	anaTok := e.token(t, anaEmail, teamGroup)
	ana := e.as(t, anaTok)
	bo := e.as(t, e.token(t, boEmail, teamGroup))

	e.expect(t, ana, "lease_claim", map[string]any{paramAgent: anaAgent, paramHost: lab, paramEnvironment: graveler, paramPurpose: purposeProof}, false, "claimed graveler")
	e.expect(t, ana, "agents_register", map[string]any{paramAgent: anaAgent, paramHost: lab}, false, "register: ana-agent idle")

	env, err := bo.read(resourceEnvironments + graveler)
	if err != nil {
		t.Fatal(err)
	}
	if h := env["status"].(map[string]any)["holder"].(map[string]any)["party"].(map[string]any); h["person"] != anaEmail {
		t.Errorf("environments/graveler: %v", env)
	}
	if _, err := bo.read(resourceLanes + portalLane); err != nil {
		t.Errorf("lanes/portal: %v", err)
	}
	roster, err := bo.read(resourceRoster)
	if err != nil {
		t.Fatal(err)
	}
	if a := roster["agents"].([]any)[0].(map[string]any); a["address"] != anaAddress || a["state"] != "idle" {
		t.Errorf("roster: %v", roster)
	}
	if _, err := ana.read(resourceNotes + anaEmail); err != nil {
		t.Errorf("ana's notes: %v", err)
	}
	if mb, err := ana.read(resourceMailbox + "Ana@Example.com"); err != nil || mb["pending"] != float64(0) {
		t.Errorf("ana's mailbox: %v, %v", mb, err)
	}
	for uri, want := range map[string]string{
		resourceNotes + anaEmail:         "only that person reads it",
		resourceMailbox + anaEmail:       "only that person reads it",
		resourceEnvironments + "nowhere": "not an Environment",
		resourceScheme + "secrets":       "resource not found",
		resourceLanes + "nowhere":        "not a merge lane",
	} {
		if _, err := bo.read(uri); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("bo reads %s: %v, want %q", uri, err, want)
		}
	}
	// A subscription is refused alike, as a whole.
	if _, err := e.listen(t, e.token(t, boEmail, teamGroup), resourceFeed, resourceMailbox+anaEmail); err == nil || !strings.Contains(err.Error(), "only that person reads it") {
		t.Errorf("bo subscribed to ana's mailbox: %v", err)
	}
	if _, err := e.listen(t, anaTok, resourceFeed, resourceMailbox+anaEmail, resourceNotes+anaEmail); err != nil {
		t.Errorf("ana's own subscription: %v", err)
	}
}

func TestEnvtestServeFeed(t *testing.T) {
	e := newServeEnv(t)
	ana := e.as(t, e.token(t, anaEmail, teamGroup))
	e.expect(t, ana, "lease_claim", map[string]any{paramAgent: anaAgent, paramHost: lab, paramEnvironment: graveler, paramPurpose: purposeProof}, false, "claimed graveler")
	e.expect(t, ana, "lease_list", map[string]any{}, false, "graveler")
	e.expect(t, ana, "lease_release", map[string]any{paramAgent: anaAgent, paramHost: lab, paramEnvironment: graveler}, false, "released graveler")

	v, err := ana.read(resourceFeed)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(v)
	var f feed.Feed
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.Schema != feed.Schema {
		t.Errorf("schema %q", f.Schema)
	}
	var kinds []string
	for i, ev := range f.Events {
		kinds = append(kinds, ev.Kind)
		if i > 0 && ev.ID <= f.Events[i-1].ID {
			t.Errorf("ids do not grow: %s after %s", ev.ID, f.Events[i-1].ID)
		}
	}
	if got := strings.Join(kinds, " "); got != "lease.claim lease.release" {
		t.Errorf("feed kinds %q: the changes, not the reads", got)
	}
	claim := f.Events[0]
	if claim.Subject != "Environment/"+graveler || claim.Actor.Person != anaEmail || claim.Actor.Host != lab ||
		!strings.HasPrefix(claim.Line, "lease.claim (ana-agent): graveler") || claim.Time.IsZero() {
		t.Errorf("the claim's event %+v", claim)
	}

	// A restart: a fresh store's ids continue above the kept Events', even
	// for an event dated before them.
	fresh, err := kube.NewForConfig(e.rc)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Audit(nil, ourTeam, state.Event{At: time.Unix(1, 0), By: state.Party{Name: "restart"}, Verb: "test.restart", Detail: "after a restart"}); err != nil {
		t.Fatal(err)
	}
	evs, err := fresh.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1]
	if last.Kind != "test.restart" || last.ID <= f.Events[len(f.Events)-1].ID {
		t.Errorf("after a restart the last event %+v, its id not above %s", last, f.Events[len(f.Events)-1].ID)
	}
}

func TestEnvtestServeSubscriptions(t *testing.T) {
	e := newServeEnv(t)
	anaTok, boTok := e.token(t, anaEmail, teamGroup), e.token(t, boEmail, teamGroup)
	ana, bo := e.as(t, anaTok), e.as(t, boTok)
	pia := e.as(t, e.token(t, "pia@example.com", "giantswarm:team-planeteers"))
	anaUp, err := e.listen(t, anaTok, resourceEnvironments+graveler, resourceLanes+portalLane, resourceNotes+anaEmail,
		resourceMailbox+anaEmail, resourceRoster, resourceFeed)
	if err != nil {
		t.Fatal(err)
	}
	boUp, err := e.listen(t, boTok, resourceNotes+boEmail, resourceMailbox+boEmail)
	if err != nil {
		t.Fatal(err)
	}

	e.expect(t, pia, "lease_claim", map[string]any{paramEnvironment: graveler, paramPurpose: purposeProof}, false, "claimed graveler")
	anaUp.expect(resourceEnvironments + graveler)
	anaUp.expect(resourceFeed)

	e.expect(t, bo, "lane_queue", map[string]any{paramRepo: backstage, "pr": 1}, false, "queued")
	anaUp.expect(resourceLanes + portalLane)

	e.expect(t, ana, "agents_register", map[string]any{paramAgent: anaAgent, paramHost: lab}, false, "register: ana-agent idle")
	anaUp.expect(resourceRoster)

	// A note reaches its addressee's and its filer's subscriptions.
	e.expect(t, bo, "note_add", map[string]any{paramText: "which lane?", paramFor: anaEmail, paramKind: noteMemo}, false, "note #1")
	anaUp.expect(resourceNotes + anaEmail)
	boUp.expect(resourceNotes + boEmail)
	e.expect(t, pia, "note_add", map[string]any{paramText: "for the team", paramFor: "team:" + ourTeam, paramKind: noteMemo}, false, "note #2")
	anaUp.expect(resourceNotes + anaEmail)
	boUp.expect(resourceNotes + boEmail)

	// A message reaches the receiver's mailbox subscription, not the sender's.
	e.expect(t, bo, "send_message", map[string]any{"to": anaAddress, paramMessage: a2a("m1", "hello")}, false, "delivery")
	anaUp.expect(resourceMailbox + anaEmail)
	e.expect(t, pia, "note_add", map[string]any{paramText: "marker", paramFor: boEmail, paramKind: noteMemo}, false, "note #3")
	boUp.expect(resourceNotes + boEmail)
	boUp.none(resourceMailbox + boEmail)
}

func a2a(id, text string) map[string]any {
	return map[string]any{"messageId": id, keyRole: "user", "parts": []any{map[string]any{"kind": "text", "text": text}}}
}

func TestEnvtestServeMessages(t *testing.T) {
	e := newServeEnv(t)
	ana := e.as(t, e.token(t, anaEmail, teamGroup))
	bo := e.as(t, e.token(t, boEmail, teamGroup))
	e.expect(t, ana, "agents_register", map[string]any{paramAgent: anaAgent, paramHost: lab}, false, "register: ana-agent idle")

	sent := e.expect(t, bo, "send_message", map[string]any{"to": anaAddress, paramMessage: a2a("m1", "hello"), paramAgent: "bo-agent", paramHost: "bo-laptop"}, false, "delivery")
	again := e.expect(t, bo, "send_message", map[string]any{"to": anaAddress, paramMessage: a2a("m1", "hello")}, false, "not queued again")
	if again["duplicate"] != true || again["id"] != sent["id"] {
		t.Errorf("the resend %v of %v", again, sent)
	}
	e.expect(t, bo, "send_message", map[string]any{"to": anaAddress, paramMessage: a2a("m2", "second")}, false, "delivery")
	e.expect(t, bo, "send_message", map[string]any{"to": "local:nowhere/nobody", paramMessage: a2a("m3", "x")}, true, "not on the roster")
	e.expect(t, bo, "send_message", map[string]any{"to": anaAddress, paramMessage: map[string]any{keyRole: "user"}}, true, "messageId and parts")
	e.expect(t, bo, "send_message", map[string]any{"to": "ana", paramMessage: a2a("m4", "x")}, true, "local:<machine>/<name> or kagent:")

	e.expect(t, bo, "receive_messages", map[string]any{paramFor: anaEmail}, true, "you read and ack only your own")
	e.expect(t, bo, "ack_messages", map[string]any{paramFor: anaEmail, "ids": []any{sent["id"]}}, true, "you read and ack only your own")
	got := e.expect(t, ana, "receive_messages", map[string]any{}, false, "message from bo@example.com: m1")
	msgs := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("received %v", got)
	}
	first := msgs[0].(map[string]any)
	env := first["envelope"].(map[string]any)
	if from := env["from"].(map[string]any); from["person"] != boEmail || from["agent"] != "bo-agent" || from["host"] != "bo-laptop" ||
		env["to"] != anaAddress || env[paramMessage].(map[string]any)["messageId"] != "m1" {
		t.Errorf("the envelope %v", env)
	}
	e.expect(t, ana, "ack_messages", map[string]any{"ids": []any{first["id"]}}, false, "acked 1 of 1")
	e.expect(t, ana, "receive_messages", map[string]any{}, false, "m2")

	// The cap: 49 more fill ana's mailbox to 50, the next is refused.
	for i := range mailbox.Cap - 1 {
		if _, _, failed := bo.call("send_message", map[string]any{"to": anaAddress, paramMessage: a2a(fmt.Sprintf("fill%d", i), "x")}); failed {
			t.Fatalf("send %d failed", i)
		}
	}
	e.expect(t, bo, "send_message", map[string]any{"to": anaAddress, paramMessage: a2a("over", "x")}, true, "the mailbox is full")

	// Expiry: past the deadline the messages go, each with an expired event
	// to its sender, on the feed and in the sender's mailbox.
	e.srv.now = func() time.Time { return time.Now().Add(mailbox.TTL + time.Hour) }
	if err := e.srv.expire(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.expect(t, ana, "receive_messages", map[string]any{}, false, "no message waits")
	notices := e.expect(t, bo, "receive_messages", map[string]any{}, false, "expired from beekeeper")
	if n := len(notices["messages"].([]any)); n != mailbox.Cap {
		t.Errorf("bo has %d expired notices, want one per expired message (%d)", n, mailbox.Cap)
	}
	evs, err := e.store.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	expired := 0
	for _, ev := range evs {
		if ev.Kind == "message.expired" {
			expired++
			if ev.Actor.Person != boEmail || ev.Subject != "Namespace/beekeeper-"+ourTeam {
				t.Errorf("expired event %+v", ev)
			}
		}
	}
	if expired != mailbox.Cap {
		t.Errorf("%d message.expired events, want %d", expired, mailbox.Cap)
	}
}

// fakeMuster is a muster that records each call_tool and its caller's token.
type fakeMuster struct {
	mu    sync.Mutex
	token string
	tool  string
	args  map[string]any
}

func newFakeMuster(t *testing.T) (*fakeMuster, string) {
	f := &fakeMuster{}
	m := mcpserver.NewMCPServer("muster", "0")
	m.AddTool(mcp.NewTool("call_tool", mcp.WithString("name"), mcp.WithObject("arguments")), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.tool = req.GetString("name", "")
		f.args, _ = req.GetArguments()["arguments"].(map[string]any)
		return mcp.NewToolResultText(`{"state":"completed","text":"done"}`), nil
	})
	h := mcpserver.NewStreamableHTTPServer(m, mcpserver.WithStateLess(true))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv.URL + "/mcp"
}

func TestEnvtestServeSendsToKagentThroughMusterAsTheCaller(t *testing.T) {
	e := newServeEnv(t)
	f, url := newFakeMuster(t)
	e.srv.cfg.Serve.Muster = url
	e.srv.cfg.Serve.Kagent = map[string]string{graveler: "x_kagent_invoke_agent_instance"}
	tok := e.token(t, anaEmail, teamGroup)
	ana := e.as(t, tok)

	got := e.expect(t, ana, "send_message", map[string]any{"to": "kagent:" + graveler + "/kagent/sess-1", paramMessage: a2a("k1", "status?")}, false, "done")
	if got["result"] != `{"state":"completed","text":"done"}` {
		t.Errorf("result %v", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.token != tok {
		t.Error("muster was not called with the caller's token")
	}
	if f.tool != "x_kagent_invoke_agent_instance" || f.args["agent_instance_id"] != "sess-1" || f.args["message"] != "status?" || f.args["message_id"] != "k1" {
		t.Errorf("muster saw %s %v", f.tool, f.args)
	}
	e.expect(t, ana, "send_message", map[string]any{"to": "kagent:glean/kagent/sess-1", paramMessage: a2a("k2", "x")}, true, "not one serve.kagent routes to")
	e.expect(t, ana, "send_message", map[string]any{"to": "kagent:" + graveler + "/sess-1", paramMessage: a2a("k3", "x")}, true, "kagent:<installation>/<namespace>/<session>")
}
