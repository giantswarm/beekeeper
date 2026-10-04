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

func client(t *testing.T, endpoint string) (*central.Client, string) {
	bin, token := centraltest.Binary(t, endpoint, "a-token")
	return central.New(config.Central{Context: "lab", Server: "beekeeper", Muster: bin, Timeout: config.Duration{Duration: 10 * time.Second}}, nil), token
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
	if _, err := c.Call(ctx, "lease_claim", map[string]any{}, nil); !errors.As(err, &u) || u.Context != "lab" || !strings.Contains(u.Reason, "does not answer") {
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
