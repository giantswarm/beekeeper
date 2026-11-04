package central_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/beekeeper/internal/central"
	"github.com/giantswarm/beekeeper/internal/central/centraltest"
)

// Losing the backend's reply after its write must not tell callers that
// they can safely repeat the write on the local machine.
func TestCallLostBackendReplyIsSent(t *testing.T) {
	var writes atomic.Int32
	s := mcpserver.NewMCPServer("beekeeper", "test")
	s.AddTool(mcp.NewTool("note_add"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		writes.Add(1)
		return mcp.NewToolResultStructured(map[string]any{"id": 41}, "note #41"), nil
	})
	h := mcpserver.NewStreamableHTTPServer(s, mcpserver.WithStateLess(true))
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Mcp-Method") == toolsCallMethod {
			// Complete the backend write, then replace its reply with a
			// truncated response, as a connection lost on the way to muster.
			h.ServeHTTP(httptest.NewRecorder(), r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{"))
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(backend.Close)
	m := centraltest.New(t, backend.URL+"/mcp", "beekeeper")
	c, _ := client(t, m.URL)
	_, err := c.Call(context.Background(), "note_add", map[string]any{}, nil)
	if n := writes.Load(); n != 1 {
		t.Fatalf("backend writes = %d, want one completed write", n)
	}
	u, ok := err.(*central.Unreachable)
	if !ok || !u.Sent {
		t.Fatalf("Call = %v (%T), want Unreachable with Sent after the backend wrote", err, err)
	}
}
