// Package centraltest gives a test a muster in front of a beekeeper serve:
// its call_tool forwards x_<server>_<tool> to the backend with the caller's
// bearer and wraps the result as muster does, and a muster binary that
// names its endpoint and prints a token.
package centraltest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

type authKey struct{}

// Muster is a test muster in front of the beekeeper serve at backend
// (its /mcp URL), registered as server.
type Muster struct {
	URL string
	srv *httptest.Server
	// down makes every call_tool fail as muster's does when the backend
	// does not answer.
	down atomic.Bool
}

// New starts a test muster forwarding to backend.
func New(t *testing.T, backend, server string) *Muster {
	t.Helper()
	m := &Muster{}
	s := mcpserver.NewMCPServer("muster", "test")
	s.AddTool(mcp.NewTool("call_tool", mcp.WithString("name", mcp.Required()), mcp.WithObject("arguments")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name := req.GetString("name", "")
			tool, ok := strings.CutPrefix(name, "x_"+server+"_")
			if !ok {
				return mcp.NewToolResultError("tool " + name + " not found"), nil
			}
			if m.down.Load() {
				return mcp.NewToolResultError("Tool execution failed: the backend does not answer"), nil
			}
			args, _ := req.GetArguments()["arguments"].(map[string]any)
			token, _ := ctx.Value(authKey{}).(string)
			res, err := forward(ctx, backend, token, tool, args)
			if err != nil {
				return mcp.NewToolResultError("Tool execution failed: " + err.Error()), nil
			}
			env, _ := json.Marshal(map[string]any{"isError": res.IsError, "content": res.Content, "structuredContent": res.StructuredContent})
			return &mcp.CallToolResult{Content: []mcp.Content{mcp.NewTextContent(string(env))}, IsError: res.IsError, StructuredContent: res.StructuredContent}, nil
		})
	h := mcpserver.NewStreamableHTTPServer(s, mcpserver.WithStateLess(true),
		mcpserver.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			return context.WithValue(ctx, authKey{}, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		}))
	m.srv = httptest.NewServer(h)
	t.Cleanup(m.srv.Close)
	m.URL = m.srv.URL + "/mcp"
	return m
}

// Down makes the backend unreachable behind muster (down) or reachable.
func (m *Muster) Down(down bool) { m.down.Store(down) }

// Close stops muster itself.
func (m *Muster) Close() { m.srv.Close() }

func forward(ctx context.Context, url, token, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	c, err := client.NewStreamableHttpClient(url, transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token}))
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	if err := c.Start(ctx); err != nil {
		return nil, err
	}
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "muster", Version: "test"}
	if _, err := c.Initialize(ctx, init); err != nil {
		return nil, err
	}
	req := mcp.CallToolRequest{}
	req.Params.Name, req.Params.Arguments = tool, args
	return c.CallTool(ctx, req)
}

// Binary writes a muster binary for one person: `context show` names
// endpoint, `auth token` prints the token file's content. It returns the
// binary and the token file, which the test may rewrite.
func Binary(t *testing.T, endpoint, token string) (bin, tokenFile string) {
	t.Helper()
	dir := t.TempDir()
	tokenFile = filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(dir, "muster")
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
context) printf '{"name":"%%s","endpoint":"%s"}' "$3" ;;
auth) cat '%s' ;;
*) echo "unknown command $1" >&2; exit 1 ;;
esac
`, endpoint, tokenFile)
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, tokenFile
}
