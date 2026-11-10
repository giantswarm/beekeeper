package central_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/beekeeper/internal/central"
)

func TestCallReusesGrantAfterAuthenticationRefusal(t *testing.T) {
	for _, tool := range []string{"note_list", "note_answer", "lease_list", "hold_list"} {
		t.Run(tool, func(t *testing.T) {
			var calls, logins, writes atomic.Int64
			s := mcpserver.NewMCPServer("muster", "test")
			s.AddTool(mcp.NewTool("call_tool"), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if req.GetString("name", "") == "core_auth_login" {
					logins.Add(1)
					args, _ := req.GetArguments()["arguments"].(map[string]any)
					if args["server"] != "beekeeper" {
						t.Errorf("login arguments = %v", args)
					}
					return mcp.NewToolResultText("Successfully connected"), nil
				}
				calls.Add(1)
				if logins.Load() == 0 {
					return centralAuthChallenge(), nil
				}
				writes.Add(1)
				return mcp.NewToolResultStructured(map[string]any{"answer": "recorded"}, "done"), nil
			})
			srv := httptest.NewServer(mcpserver.NewStreamableHTTPServer(s, mcpserver.WithStateLess(true)))
			defer srv.Close()
			c, _ := client(t, srv.URL+"/mcp")
			var result struct{ Answer string }
			text, err := c.Call(t.Context(), tool, map[string]any{"note": 144}, &result)
			if err != nil || text != "done" || result.Answer != "recorded" || calls.Load() != 2 || logins.Load() != 1 || writes.Load() != 1 {
				t.Fatalf("call = %q, %+v, %v; calls/logins/writes = %d/%d/%d", text, result, err, calls.Load(), logins.Load(), writes.Load())
			}
		})
	}
}

func centralAuthChallenge() *mcp.CallToolResult {
	res := mcp.NewToolResultError("auth_required: server 'beekeeper' requires authentication before its tools can be called (this session is not authenticated to it)")
	res.StructuredContent = map[string]any{"status": "auth_required", "server": "beekeeper"}
	return res
}

func TestCallAuthenticationRecoveryIsBounded(t *testing.T) {
	challenge := mcp.NewToolResultText("not Successfully connected: sign in first")
	challenge.StructuredContent = map[string]any{"authUrl": "https://example.invalid/sign-in?private=challenge"}
	for _, tc := range []struct {
		name       string
		first      *mcp.CallToolResult
		login      *mcp.CallToolResult
		wantCalls  int64
		wantLogins int64
		wantSent   bool
	}{
		{"browser required", centralAuthChallenge(), mcp.NewToolResultText("Open https://example.invalid/sign-in?private=challenge"), 1, 1, false},
		{"structured challenge", centralAuthChallenge(), challenge, 1, 1, false},
		{"login unavailable", centralAuthChallenge(), mcp.NewToolResultError("tool not found"), 1, 1, false},
		{"still refused", centralAuthChallenge(), mcp.NewToolResultText("Already Connected"), 2, 1, false},
		{"legacy refusal", mcp.NewToolResultError("auth_required: server 'beekeeper' requires authentication before its tools can be called"), mcp.NewToolResultText("already authenticated"), 2, 1, false},
		{"ambiguous failure", mcp.NewToolResultError("backend lost its reply: auth_required"), nil, 1, 0, true},
		{"other server", mcp.NewToolResultError("auth_required: server 'another' requires authentication before its tools can be called"), nil, 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls, logins atomic.Int64
			s := mcpserver.NewMCPServer("muster", "test")
			s.AddTool(mcp.NewTool("call_tool"), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if req.GetString("name", "") == "core_auth_login" {
					logins.Add(1)
					return tc.login, nil
				}
				calls.Add(1)
				return tc.first, nil
			})
			srv := httptest.NewServer(mcpserver.NewStreamableHTTPServer(s, mcpserver.WithStateLess(true)))
			defer srv.Close()
			c, _ := client(t, srv.URL+"/mcp")
			text, err := c.Call(t.Context(), "note_answer", nil, nil)
			var unreachable *central.Unreachable
			if !errors.As(err, &unreachable) || unreachable.Sent != tc.wantSent || calls.Load() != tc.wantCalls || logins.Load() != tc.wantLogins {
				t.Fatalf("call = %q, %v; calls/logins = %d/%d", text, err, calls.Load(), logins.Load())
			}
			if !tc.wantSent && (!strings.Contains(err.Error(), "muster auth login --context lab --server beekeeper") || strings.Contains(err.Error()+text, "private=challenge")) {
				t.Fatalf("missing recovery command or leaked challenge: %q, %v", text, err)
			}
		})
	}
}
