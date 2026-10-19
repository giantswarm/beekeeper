package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/beekeeper/internal/central/centraltest"
	"github.com/giantswarm/beekeeper/internal/mailbox"
)

// The person, machine, agent and central server of the mailbox tests.
const (
	mailPerson = "ada@example.com"
	mailHost   = "desk"
	mailGuide  = "Guide"
	mailServer = "beekeeper"
)

// mailboxStandIn is beekeeper serve's mailbox tools: receive_messages
// returns the unacked deliveries, ack_messages acks them.
type mailboxStandIn struct {
	mu    sync.Mutex
	queue []mailbox.Delivery
	acked []int64
	said  []string
}

func (m *mailboxStandIn) url(t *testing.T) string {
	srv := mcpserver.NewMCPServer(mailServer, "test")
	srv.AddTool(mcp.NewTool("receive_messages"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		ds := slices.DeleteFunc(slices.Clone(m.queue), func(d mailbox.Delivery) bool { return slices.Contains(m.acked, d.Seq) })
		return mcp.NewToolResultStructured(map[string]any{keyMailbox: mailPerson, "messages": ds}, fmt.Sprintf("%d messages", len(ds))), nil
	})
	srv.AddTool(mcp.NewTool("ack_messages"), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, id := range req.GetIntSlice("ids", nil) {
			m.acked = append(m.acked, int64(id))
		}
		return mcp.NewToolResultText("acked"), nil
	})
	srv.AddTool(mcp.NewTool("converse"), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.said = append(m.said, req.GetString(paramAgent, "")+"@"+req.GetString(paramHost, "")+": "+req.GetString(paramText, ""))
		return mcp.NewToolResultText("said in conversation C1"), nil
	})
	h := httptest.NewServer(mcpserver.NewStreamableHTTPServer(srv, mcpserver.WithStateLess(true)))
	t.Cleanup(h.Close)
	return h.URL + "/mcp"
}

// slackReply is a delivery of the person's reply in Slack to the agent at
// to.
func slackReply(t *testing.T, seq int64, to, text string) mailbox.Delivery {
	t.Helper()
	id := fmt.Sprintf("slack-D1-%d", seq)
	var env mailEnvelope
	env.To, env.From.Person, env.From.Agent, env.From.Source = to, mailPerson, mailPerson, viaSlack
	env.Message.MessageID = id
	env.Message.Parts = []a2aPart{{Text: text}}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return mailbox.Delivery{Seq: seq, Kind: mailbox.KindMessage, Sender: mailPerson, MessageID: id, Envelope: raw}
}

// mailboxWatch is a standby watch on host desk whose central instance is
// the stand-in mb behind a test muster; sent collects the deliveries, a
// target in refuse does not take them.
func mailboxWatch(t *testing.T, mb *mailboxStandIn, refuse ...string) (*watcher, *[]string, func() string) {
	t.Helper()
	w, sent, out, _ := mailboxWatchVia(t, mb, refuse...)
	return w, sent, out
}

// mailboxWatchVia is mailboxWatch with its muster.
func mailboxWatchVia(t *testing.T, mb *mailboxStandIn, refuse ...string) (*watcher, *[]string, func() string, *centraltest.Muster) {
	t.Helper()
	w, _, out := notifyingWatch(t, t.TempDir(), true)
	w.notifier = nil
	m := centraltest.New(t, mb.url(t), mailServer)
	bin, _ := centraltest.Binary(t, m.URL, "token")
	w.cfg.Central.Context, w.cfg.Central.Server = lab, mailServer
	w.cfg.Central.Muster, w.cfg.Central.Timeout.Duration = bin, 10*time.Second
	w.cfg.Identity.Person, w.cfg.Identity.Host = "Ada@example.com", mailHost
	var sent []string
	w.deliver = func(_ context.Context, q, msg string) error {
		if slices.Contains(refuse, q) {
			return errors.New(q + " has no running CLI")
		}
		sent = append(sent, q+" <- "+msg)
		return nil
	}
	return w, &sent, out.String, m
}

func TestMailboxDeliversToTheMachinesAgents(t *testing.T) {
	mb := &mailboxStandIn{}
	expired, _ := json.Marshal(mailEnvelope{To: "bob@example.com", MessageID: "m-7", Deadline: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)})
	mb.queue = []mailbox.Delivery{
		slackReply(t, 1, "local:desk/Guide", "take option 2"),
		slackReply(t, 2, "local:laptop/Guide", "for the laptop"),
		{Seq: 3, Kind: mailbox.KindExpired, Sender: mailServer, MessageID: "expired-7", Envelope: expired},
		slackReply(t, 4, "local:DESK/Guide", "and go on"),
	}
	w, sent, out := mailboxWatch(t, mb)
	if w.drainMailbox(context.Background()) {
		t.Error("drain says a message waits")
	}
	want := []string{
		"Guide <- Message from ada@example.com via slack (slack-D1-1): take option 2\n\nThey wrote it in your Slack thread with them: answer there with `beekeeper converse \"<text>\"`.",
		"Guide <- Message from ada@example.com via slack (slack-D1-4): and go on\n\nThey wrote it in your Slack thread with them: answer there with `beekeeper converse \"<text>\"`.",
	}
	if strings.Join(*sent, "\n---\n") != strings.Join(want, "\n---\n") {
		t.Errorf("sent:\n%s", strings.Join(*sent, "\n---\n"))
	}
	if !slices.Equal(mb.acked, []int64{1, 3, 4}) {
		t.Errorf("acked %v, want 1 3 4: the laptop's message is the laptop's", mb.acked)
	}
	for _, line := range []string{"MAILBOX to Guide: slack-D1-1 from ada@example.com via slack: take option 2", "MAILBOX: the message m-7 to bob@example.com expired unacked"} {
		if !strings.Contains(out(), line) {
			t.Errorf("no %q in:\n%s", line, out())
		}
	}
	*sent = nil
	if w.drainMailbox(context.Background()); len(*sent) != 0 {
		t.Errorf("delivered again: %v", *sent)
	}
}

func TestMailboxKeepsWhatItsAgentDoesNotTake(t *testing.T) {
	mb := &mailboxStandIn{}
	mb.queue = []mailbox.Delivery{
		slackReply(t, 1, "local:desk/Guide", "first"),
		slackReply(t, 2, "local:desk/Worker", "for the worker"),
		slackReply(t, 3, "local:desk/Guide", "second"),
	}
	w, sent, out := mailboxWatch(t, mb, "Guide")
	if !w.drainMailbox(context.Background()) {
		t.Error("drain says nothing waits")
	}
	if len(*sent) != 1 || !strings.HasPrefix((*sent)[0], "Worker <- ") || !slices.Equal(mb.acked, []int64{2}) {
		t.Errorf("sent %v, acked %v: want only the worker's", *sent, mb.acked)
	}
	if n := strings.Count(out(), "MAILBOX slack-D1-1 waits for Guide"); n != 1 {
		t.Errorf("%d waits lines, want one, the first message's:\n%s", n, out())
	}
	// Once the guide takes them, both arrive in their order.
	w.deliver = func(_ context.Context, q, msg string) error {
		*sent = append(*sent, q+" <- "+firstLine(msg))
		return nil
	}
	*sent = nil
	if w.drainMailbox(context.Background()) {
		t.Error("drain says a message still waits")
	}
	if strings.Join(*sent, "|") != "Guide <- Message from ada@example.com via slack (slack-D1-1): first|Guide <- Message from ada@example.com via slack (slack-D1-3): second" {
		t.Errorf("sent %v", *sent)
	}
	if !strings.Contains(out(), "ENDED") {
		t.Errorf("the wait is not ended:\n%s", out())
	}
}

func TestConverseSaysItAsTheCallingAgent(t *testing.T) {
	mb := &mailboxStandIn{}
	w, _, _ := mailboxWatch(t, mb)
	var out strings.Builder
	a := *w.app
	a.out, a.as = &out, mailGuide
	if err := a.converse("which one, 1 or 2?"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(mb.said, []string{"Guide@desk: which one, 1 or 2?"}) || out.String() != "said in conversation C1\n" {
		t.Errorf("said %v, printed %q", mb.said, out.String())
	}
	a.cfg.Central.Context = ""
	if err := a.converse("hello"); err == nil || !strings.Contains(err.Error(), "no central instance") {
		t.Errorf("converse without central = %v", err)
	}
}

func TestMailboxWatchDrainsOnEachUpdate(t *testing.T) {
	mb := &mailboxStandIn{queue: []mailbox.Delivery{slackReply(t, 1, "local:desk/Guide", "first")}}
	w, _, out, m := mailboxWatchVia(t, mb)
	w.cfg.Watch.Interval.Duration = time.Hour
	delivered := make(chan string, 10)
	w.deliver = func(_ context.Context, _, msg string) error {
		delivered <- firstLine(msg)
		return nil
	}
	expect := func(want string) {
		t.Helper()
		select {
		case got := <-delivered:
			if !strings.HasSuffix(got, want) {
				t.Fatalf("delivered %q, want %s", got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s not delivered:\n%s", want, out())
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.watchMailbox(ctx)
	}()
	// The subscription's acknowledgement drains what waited before it.
	expect(": first")
	mb.mu.Lock()
	mb.queue = append(mb.queue, slackReply(t, 2, "local:desk/Guide", "second"))
	mb.mu.Unlock()
	if m.Updated(resourceMailbox+mailPerson) != 1 {
		t.Fatal("no stream subscribed to the person's mailbox")
	}
	expect(": second")
	cancel()
	<-done
	if !slices.Equal(mb.acked, []int64{1, 2}) {
		t.Errorf("acked %v", mb.acked)
	}
}

func TestMailBodyNamesAnAgentSender(t *testing.T) {
	d := slackReply(t, 1, "local:desk/Guide", "merge it")
	var env mailEnvelope
	if err := json.Unmarshal(d.Envelope, &env); err != nil {
		t.Fatal(err)
	}
	env.From.Agent, env.From.Host = "Worker", "laptop"
	if got := mailBody(d, env); got != "Message from ada@example.com's agent Worker on laptop (slack-D1-1): merge it" {
		t.Errorf("body %q: an agent's message passes as the person's", got)
	}
}
