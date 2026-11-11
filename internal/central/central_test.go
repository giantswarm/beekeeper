package central_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/central"
	"github.com/giantswarm/beekeeper/internal/central/centraltest"
	"github.com/giantswarm/beekeeper/internal/config"
)

// backend is a beekeeper serve stand-in: its results carry the structured
// outcome beekeeper serve puts on a failure.
func backend(t *testing.T) string {
	s := mcpserver.NewMCPServer("beekeeper", "test")
	s.AddTool(mcp.NewTool("lease_claim"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultStructured(map[string]any{"environment": "graveler", "purpose": "e2e"}, "claimed graveler"), nil
	})
	fail := func(outcome, text string) mcpserver.ToolHandlerFunc {
		return func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			res := mcp.NewToolResultError(text)
			res.StructuredContent = map[string]string{"outcome": outcome, "message": text}
			return res, nil
		}
	}
	s.AddTool(mcp.NewTool("lease_release"), fail("refused", `graveler is held by "ana/agent" since 10:02: e2e`))
	s.AddTool(mcp.NewTool("lanes"), fail("error", "the store does not answer"))
	srv := httptest.NewServer(mcpserver.NewStreamableHTTPServer(s, mcpserver.WithStateLess(true)))
	t.Cleanup(srv.Close)
	return srv.URL + "/mcp"
}

// labContext and beekeeperServer are the muster context and server the tests call through.
const (
	labContext      = "lab"
	beekeeperServer = "beekeeper"
)

func client(t *testing.T, endpoint string) (*central.Client, string) {
	return clientOf(t, endpoint, beekeeperServer, 10*time.Second)
}

func clientOf(t *testing.T, endpoint, server string, timeout time.Duration) (*central.Client, string) {
	bin, token := centraltest.Binary(t, endpoint, "a-token")
	return central.New(config.Central{Context: labContext, Server: server, Muster: bin, Timeout: config.Duration{Duration: timeout}}, nil), token
}

func TestCall(t *testing.T) {
	m := centraltest.New(t, backend(t), "beekeeper")
	c, token := client(t, m.URL)
	ctx := context.Background()

	var v struct{ Environment, Purpose string }
	text, err := c.Call(ctx, "lease_claim", map[string]any{}, &v)
	if err != nil || text != "claimed graveler" || v.Environment != "graveler" || v.Purpose != "e2e" {
		t.Errorf("lease_claim = %q, %+v, %v", text, v, err)
	}

	var r *central.Refused
	if _, err := c.Call(ctx, "lease_release", map[string]any{}, nil); !errors.As(err, &r) || !strings.Contains(r.Message, `held by "ana/agent"`) {
		t.Errorf("lease_release = %v, want the refusal", err)
	}

	var u *central.Unreachable
	if _, err := c.Call(ctx, "lanes", map[string]any{}, nil); err == nil || errors.As(err, &r) || errors.As(err, &u) {
		t.Errorf("lanes = %v (%T), want beekeeper's error", err, err)
	}

	// Each way of not reaching beekeeper serve is Unreachable, naming the
	// context and why.
	m.Down(true)
	if _, err := c.Call(ctx, "lease_claim", map[string]any{}, nil); !errors.As(err, &u) || !u.Sent || u.Context != labContext || !strings.Contains(u.Reason, "does not answer") {
		t.Errorf("backend down: %v, want Unreachable", err)
	}
	m.Down(false)
	if err := os.Remove(token); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, "lease_claim", map[string]any{}, nil); !errors.As(err, &u) || u.Sent || !strings.Contains(u.Reason, "muster auth login --context lab") {
		t.Errorf("no sign-in: %v, want Unreachable naming the login", err)
	}
	gone, _ := client(t, m.URL)
	m.Close()
	if _, err := gone.Call(ctx, "lease_claim", map[string]any{}, nil); !errors.As(err, &u) || u.Sent {
		t.Errorf("muster down: %v, want Unreachable", err)
	}
}

// Losing the backend reply after a write can still produce a successful MCP
// exchange with muster, whose generic error cannot establish whether it acted.
func TestCallLostBackendReplyIsSent(t *testing.T) {
	var writes atomic.Int64
	s := mcpserver.NewMCPServer("beekeeper", "test")
	s.AddTool(mcp.NewTool("note_add"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		writes.Add(1)
		return mcp.NewToolResultStructured(map[string]any{"id": 41}, "note #41"), nil
	})
	h := mcpserver.NewStreamableHTTPServer(s, mcpserver.WithStateLess(true))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if bytes.Contains(body, []byte(`"note_add"`)) {
			// Run the real handler to completion, then lose its successful reply.
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n{", rec.Body.Len()+100)
			_ = rw.Flush()
			return
		}
		for k, vs := range rec.Header() {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	defer srv.Close()
	m := centraltest.New(t, srv.URL+"/mcp", "beekeeper")
	c, _ := client(t, m.URL)
	_, err := c.Call(context.Background(), "note_add", map[string]any{}, nil)
	if got := writes.Load(); got != 1 {
		t.Fatalf("backend writes = %d, want 1", got)
	}
	var u *central.Unreachable
	if !errors.As(err, &u) || !u.Sent {
		t.Fatalf("lost backend reply = %v (%T), want Unreachable with Sent=true", err, err)
	}
}

// A connection that stalls times the call out naming the stage and its
// deadline; the next call connects afresh and is answered. Allow process
// startup and TLS handshakes margin on busy CI runners; only the deliberately
// stalled request should exhaust the deadline.
func TestCallStalledConnection(t *testing.T) {
	for method, stage := range map[string]string{"": "connect", "tools/call": "tool call"} {
		t.Run(stage, func(t *testing.T) {
			m, tr := centraltest.NewStalling(t, backend(t), "beekeeper", method)
			central.SetBaseTransport(t, tr)
			c, _ := clientOf(t, m.URL, "beekeeper", 5*time.Second)
			ctx := context.Background()

			var u *central.Unreachable
			if _, err := c.Call(ctx, "lease_claim", map[string]any{}, nil); !errors.As(err, &u) || u.Reason != stage+" timed out (deadline 5s)" {
				t.Fatalf("stalled: %v, want Unreachable naming the %s stage and its deadline", err, stage)
			}
			if u.Sent != (method == "tools/call") {
				t.Errorf("stalled in the %s stage: Sent %v, want it only once the tool call went out", stage, u.Sent)
			}
			if text, err := c.Call(ctx, "lease_claim", map[string]any{}, nil); err != nil || text != "claimed graveler" {
				t.Errorf("after the stall: %q, %v, want the answer over a fresh connection", text, err)
			}
		})
	}
}

// A muster auth token that does not return times the call out in auth.
func TestCallAuthTimeout(t *testing.T) {
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "context" {
			return []byte(`{"endpoint":"https://muster.invalid/mcp"}`), nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	c := central.New(config.Central{Context: labContext, Server: beekeeperServer, Timeout: config.Duration{Duration: 200 * time.Millisecond}}, run)
	var u *central.Unreachable
	if _, err := c.Call(context.Background(), "lease_claim", map[string]any{}, nil); !errors.As(err, &u) || u.Reason != "auth timed out (deadline 200ms)" {
		t.Errorf("auth stalled: %v, want Unreachable naming the auth stage and its deadline", err)
	}
}

// pagerDuty is muster's PagerDuty server stand-in: no open incident.
func pagerDuty(t *testing.T) string {
	s := mcpserver.NewMCPServer("pagerduty", "test")
	s.AddTool(mcp.NewTool("list_incidents"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText(`{"response": []}`), nil
	})
	srv := httptest.NewServer(mcpserver.NewStreamableHTTPServer(s, mcpserver.WithStateLess(true)))
	t.Cleanup(srv.Close)
	return srv.URL + "/mcp"
}

// With muster answering, a stalled connection costs the PagerDuty reading
// one attempt: its retry connects afresh and reads.
func TestPagerDutyReadStalledConnection(t *testing.T) {
	m, tr := centraltest.NewStalling(t, pagerDuty(t), "pd", "tools/call")
	central.SetBaseTransport(t, tr)
	c, _ := clientOf(t, m.URL, "pd", 5*time.Second)
	var calls []error
	r := alerts.PagerDutyReader{Services: []string{"PSVC"}, Call: func(ctx context.Context, tool string, args map[string]any) (string, error) {
		text, err := c.Call(ctx, tool, args, nil)
		calls = append(calls, err)
		return text, err
	}}
	if ans := r.Read(context.Background(), nil); !ans.OK || len(ans.Incidents) != 0 {
		t.Errorf("read = %+v, want no open incident", ans)
	}
	if len(calls) != 2 || calls[0] == nil || !strings.Contains(calls[0].Error(), "tool call timed out (deadline 5s)") || calls[1] != nil {
		t.Errorf("attempts %v, want the stalled tool call, then the answer", calls)
	}
}

// mailboxURI is the resource the listen tests subscribe to.
const mailboxURI = "beekeeper://mailbox/ada@example.com"

// subscriber is a beekeeper serve stand-in that serves subscriptions/listen
// for mailboxURI alone, to the bearer a-token, and sends one update of it
// when the test observes the acknowledged stream.
func subscriber(t *testing.T, updates <-chan struct{}) string {
	t.Helper()
	type bearerKey struct{}
	hooks := &mcpserver.Hooks{}
	hooks.AddOnRequestInitialization(func(ctx context.Context, _ any, message any) error {
		raw, _ := json.Marshal(message)
		var req struct {
			Method string                        `json:"method"`
			Params mcp.SubscriptionsListenParams `json:"params"`
		}
		if json.Unmarshal(raw, &req) != nil || req.Method != string(mcp.MethodSubscriptionsListen) {
			return nil
		}
		if ctx.Value(bearerKey{}) != "a-token" {
			return errors.New("not the person's bearer")
		}
		for _, uri := range req.Params.Notifications.ResourceSubscriptions {
			if uri != mailboxURI {
				return errors.New("the mailbox of another person")
			}
		}
		return nil
	})
	hooks.AddBeforeSubscriptionsListen(func(ctx context.Context, id any, _ *mcp.SubscriptionsListenRequest) {
		session := mcpserver.ClientSessionFromContext(ctx)
		if updates == nil {
			return
		}
		go func() {
			select {
			case <-updates:
			case <-ctx.Done():
				return
			}
			select {
			case session.NotificationChannel() <- mcp.JSONRPCNotification{JSONRPC: mcp.JSONRPC_VERSION, Notification: mcp.Notification{
				Method: mcp.MethodNotificationResourceUpdated,
				Params: mcp.NotificationParams{Meta: map[string]any{mcp.MetaKeySubscriptionID: id}, AdditionalFields: map[string]any{"uri": mailboxURI}},
			}}:
			case <-ctx.Done():
			}
		}()
	})
	s := mcpserver.NewMCPServer("beekeeper", "test", mcpserver.WithResourceCapabilities(true, false), mcpserver.WithHooks(hooks))
	srv := httptest.NewServer(mcpserver.NewStreamableHTTPServer(s, mcpserver.WithStateLess(true),
		mcpserver.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			return context.WithValue(ctx, bearerKey{}, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		})))
	t.Cleanup(srv.Close)
	return srv.URL + "/mcp"
}

func TestListen(t *testing.T) {
	updates := make(chan struct{})
	c, _ := client(t, subscriber(t, updates))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan string, 2)
	done := make(chan error, 1)
	go func() {
		done <- c.Listen(ctx, []string{mailboxURI}, func() { events <- "ready" }, func(uri string) { events <- uri })
	}()
	for _, want := range []string{"ready", mailboxURI} {
		select {
		case event := <-events:
			if event != want {
				t.Fatalf("event %q, want %s", event, want)
			}
			if event == "ready" {
				close(updates)
			}
		case err := <-done:
			t.Fatalf("Listen ended before %s: %v", want, err)
		case <-time.After(10 * time.Second):
			t.Fatalf("no %s", want)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Listen after its context ended = %v, want nil", err)
	}
}

func TestListenRefused(t *testing.T) {
	c, _ := client(t, subscriber(t, nil))
	err := c.Listen(context.Background(), []string{"beekeeper://mailbox/bob@example.com"}, func() { t.Error("acknowledged") }, func(string) {})
	var r *central.Refused
	if !errors.As(err, &r) || !strings.Contains(r.Message, "another person") {
		t.Errorf("Listen = %v (%T), want the refusal", err, err)
	}
}

func TestListenUnreachable(t *testing.T) {
	c, _ := client(t, "http://127.0.0.1:1/mcp")
	var u *central.Unreachable
	if err := c.Listen(context.Background(), []string{mailboxURI}, func() {}, func(string) {}); !errors.As(err, &u) || u.Context != labContext {
		t.Errorf("Listen = %v (%T), want Unreachable", err, err)
	}
}

// A tool the instance does not serve is a version mismatch, named as such:
// the instance answered and nothing ran.
func TestToolNotFoundIsNoOutage(t *testing.T) {
	m := centraltest.New(t, backend(t), "beekeeper")
	c, _ := client(t, m.URL)
	_, err := c.Call(context.Background(), "note_list_v9", map[string]any{}, nil)
	var u *central.Unreachable
	if !errors.As(err, &u) || u.Sent || u.Missing != "x_beekeeper_note_list_v9" {
		t.Fatalf("got %#v (%v), want the missing tool, unsent", err, err)
	}
	if msg := err.Error(); strings.Contains(msg, "unreachable") || !strings.Contains(msg, "another version") {
		t.Fatalf("got %q, want the mismatch named", msg)
	}
}

// Both tool calls and subscriptions must fail before invoking muster.
func TestSandboxHostOnly(t *testing.T) {
	t.Setenv("BEEKEEPER_SANDBOX", "1")
	t.Setenv("BEEKEEPER_SANDBOX_BROKERED", "")
	c := central.New(config.Central{Context: labContext, Timeout: config.Duration{Duration: time.Second}}, func(context.Context, ...string) ([]byte, error) {
		t.Fatal("sandboxed central access invoked muster")
		return nil, nil
	})
	for _, op := range []string{"call", "listen"} {
		t.Run(op, func(t *testing.T) {
			var err error
			if op == "call" {
				_, err = c.Call(context.Background(), "note_list", map[string]any{}, nil)
			} else {
				err = c.Listen(context.Background(), []string{"beekeeper://feed"}, func() {}, func(string) {})
			}
			var u *central.Unreachable
			if !errors.As(err, &u) || u.Sent || !strings.Contains(err.Error(), "central access is host-only") {
				t.Fatalf("got %v, want unsent host-only Unreachable", err)
			}
		})
	}
}
