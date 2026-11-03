// Package centraltest gives a test a muster in front of a beekeeper serve:
// its call_tool forwards x_<server>_<tool> to the backend with the caller's
// bearer and wraps the result as muster does, its subscriptions/listen
// streams take the updates the test relays, and a muster binary that names
// its endpoint and prints a token.
package centraltest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	// streams are the subscriptions/listen streams open on muster.
	mu      sync.Mutex
	streams map[*stream]struct{}
}

// stream is one subscriptions/listen stream: its session, the request id
// its notifications carry, and the URIs it subscribed.
type stream struct {
	session mcpserver.ClientSession
	id      any
	uris    []string
}

// New starts a test muster forwarding to backend.
func New(t *testing.T, backend, server string) *Muster {
	t.Helper()
	m := &Muster{}
	m.srv = httptest.NewServer(m.handler(backend, server))
	t.Cleanup(m.srv.Close)
	m.URL = m.srv.URL + "/mcp"
	return m
}

type connKey struct{}

// NewStalling starts a test muster over HTTP/2, as muster behind its
// ingress answers, whose first connection stalls: it takes the requests of
// the JSON-RPC method on it and never answers them, as a connection whose
// far end went away without a word; method "" stalls every request on it.
// Every other connection answers. It returns the muster and the transport
// that trusts it.
func NewStalling(t *testing.T, backend, server, method string) (*Muster, *http.Transport) {
	t.Helper()
	m := &Muster{}
	h := m.handler(backend, server)
	var conns atomic.Int64
	m.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(connKey{}) == int64(1) && r.Body != nil {
			body, _ := io.ReadAll(r.Body)
			var msg struct {
				Method string `json:"method"`
			}
			if json.Unmarshal(body, &msg) == nil && (method == "" || msg.Method == method) {
				<-r.Context().Done()
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		h.ServeHTTP(w, r)
	}))
	m.srv.EnableHTTP2 = true
	m.srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, connKey{}, conns.Add(1))
	}
	m.srv.StartTLS()
	t.Cleanup(m.srv.Close)
	m.URL = m.srv.URL + "/mcp"
	return m, m.srv.Client().Transport.(*http.Transport)
}

// handler is muster's MCP endpoint in front of backend.
func (m *Muster) handler(backend, server string) http.Handler {
	hooks := &mcpserver.Hooks{}
	hooks.AddBeforeSubscriptionsListen(func(ctx context.Context, id any, req *mcp.SubscriptionsListenRequest) {
		st := &stream{session: mcpserver.ClientSessionFromContext(ctx), id: id, uris: req.Params.Notifications.ResourceSubscriptions}
		m.mu.Lock()
		if m.streams == nil {
			m.streams = map[*stream]struct{}{}
		}
		m.streams[st] = struct{}{}
		m.mu.Unlock()
		go func() {
			<-ctx.Done()
			m.mu.Lock()
			delete(m.streams, st)
			m.mu.Unlock()
		}()
	})
	s := mcpserver.NewMCPServer("muster", "test", mcpserver.WithResourceCapabilities(true, false), mcpserver.WithHooks(hooks))
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
	return mcpserver.NewStreamableHTTPServer(s, mcpserver.WithStateLess(true),
		mcpserver.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			return context.WithValue(ctx, authKey{}, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		}))
}

// Updated sends notifications/resources/updated of uri to every stream
// subscribed to it, as muster relays the backend's, and says how many it
// reached.
func (m *Muster) Updated(uri string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for st := range m.streams {
		if !slices.Contains(st.uris, uri) {
			continue
		}
		st.session.NotificationChannel() <- mcp.JSONRPCNotification{JSONRPC: mcp.JSONRPC_VERSION, Notification: mcp.Notification{
			Method: mcp.MethodNotificationResourceUpdated,
			Params: mcp.NotificationParams{Meta: map[string]any{mcp.MetaKeySubscriptionID: st.id}, AdditionalFields: map[string]any{"uri": uri}},
		}}
		n++
	}
	return n
}

// Down makes the backend unreachable behind muster (down) or reachable.
func (m *Muster) Down(down bool) { m.down.Store(down) }

// Close stops muster itself.
func (m *Muster) Close() { m.srv.Close() }

// dial is an initialized client of the backend at url as token's person.
func dial(ctx context.Context, url, token string) (*client.Client, error) {
	c, err := client.NewStreamableHttpClient(url, transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token}))
	if err != nil {
		return nil, err
	}
	if err := c.Start(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "muster", Version: "test"}
	if _, err := c.Initialize(ctx, init); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func forward(ctx context.Context, url, token, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	c, err := dial(ctx, url, token)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
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
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // an executable under the test's temporary folder
		t.Fatal(err)
	}
	return bin, tokenFile
}
