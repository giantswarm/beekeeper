package central_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
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

// labContext is the muster context the tests call through.
const labContext = "lab"

func client(t *testing.T, endpoint string) (*central.Client, string) {
	return clientOf(t, endpoint, "beekeeper", 10*time.Second)
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
	if _, err := c.Call(ctx, "lease_claim", map[string]any{}, nil); !errors.As(err, &u) || u.Context != labContext || !strings.Contains(u.Reason, "does not answer") {
		t.Errorf("backend down: %v, want Unreachable", err)
	}
	m.Down(false)
	if err := os.Remove(token); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, "lease_claim", map[string]any{}, nil); !errors.As(err, &u) || !strings.Contains(u.Reason, "muster auth login --context lab") {
		t.Errorf("no sign-in: %v, want Unreachable naming the login", err)
	}
	gone, _ := client(t, m.URL)
	m.Close()
	if _, err := gone.Call(ctx, "lease_claim", map[string]any{}, nil); !errors.As(err, &u) {
		t.Errorf("muster down: %v, want Unreachable", err)
	}
}

// A connection that stalls times the call out naming the stage and its
// deadline; the next call connects afresh and is answered.
func TestCallStalledConnection(t *testing.T) {
	for method, stage := range map[string]string{"": "connect", "tools/call": "tool call"} {
		t.Run(stage, func(t *testing.T) {
			m, tr := centraltest.NewStalling(t, backend(t), "beekeeper", method)
			central.SetBaseTransport(t, tr)
			c, _ := clientOf(t, m.URL, "beekeeper", 500*time.Millisecond)
			ctx := context.Background()

			var u *central.Unreachable
			if _, err := c.Call(ctx, "lease_claim", map[string]any{}, nil); !errors.As(err, &u) || u.Reason != stage+" timed out (deadline 500ms)" {
				t.Fatalf("stalled: %v, want Unreachable naming the %s stage and its deadline", err, stage)
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
	c := central.New(config.Central{Context: labContext, Server: "beekeeper", Timeout: config.Duration{Duration: 200 * time.Millisecond}}, run)
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
	c, _ := clientOf(t, m.URL, "pd", 500*time.Millisecond)
	var calls []error
	r := alerts.PagerDutyReader{Services: []string{"PSVC"}, Call: func(ctx context.Context, tool string, args map[string]any) (string, error) {
		text, err := c.Call(ctx, tool, args, nil)
		calls = append(calls, err)
		return text, err
	}}
	if ans := r.Read(context.Background(), nil); !ans.OK || len(ans.Incidents) != 0 {
		t.Errorf("read = %+v, want no open incident", ans)
	}
	if len(calls) != 2 || calls[0] == nil || !strings.Contains(calls[0].Error(), "tool call timed out (deadline 500ms)") || calls[1] != nil {
		t.Errorf("attempts %v, want the stalled tool call, then the answer", calls)
	}
}
