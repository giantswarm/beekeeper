package centraltest

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestCloseActiveSubscription(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		name := "HTTP"
		if h2 {
			name = "HTTP2"
		}
		for _, cleanup := range []bool{false, true} {
			mode := "Close"
			if cleanup {
				mode = "Cleanup"
			}
			t.Run(name+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				// Release the synthetic client even if shutdown regresses, so neither
				// the reproducer nor its registered cleanup can hang the test suite.
				timedOut := make(chan struct{})
				watchdog := time.AfterFunc(3*time.Second, func() { close(timedOut); cancel() })
				defer watchdog.Stop()
				t.Run("fixture", func(t *testing.T) {
					finished := make(chan struct{})
					t.Cleanup(func() {
						select {
						case <-finished:
						case <-time.After(time.Second):
							t.Error("subscriber did not disconnect")
						}
					})
					var m *Muster
					c := &http.Client{}
					if h2 {
						var tr *http.Transport
						m, tr = NewStalling(t, "", "", "never-stall")
						c.Transport = tr
						defer tr.CloseIdleConnections()
					} else {
						m = New(t, "", "")
					}
					req := listenRequest(t, ctx, m.URL)
					resp, err := c.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					if h2 && resp.ProtoMajor != 2 {
						t.Fatal("expected HTTP/2")
					}
					// Read the acknowledgement before closing, proving the handler is active.
					scanner := bufio.NewScanner(resp.Body)
					for scanner.Scan() {
						if strings.HasPrefix(scanner.Text(), "data: ") {
							break
						}
					}
					if scanner.Err() != nil {
						t.Fatal(scanner.Err())
					}
					if !strings.Contains(scanner.Text(), `notifications/subscriptions/acknowledged`) {
						t.Fatalf("missing acknowledgement: %q", scanner.Text())
					}
					go func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close(); close(finished) }()
					if !cleanup {
						m.Close()
						m.Close()
					}
				})
				select {
				case <-timedOut:
					t.Fatal("Close waited for the subscriber to cancel")
				default:
				}
			})
		}
	}
}

func TestCloseStalledRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, tr := NewStalling(t, "", "", "")
	defer tr.CloseIdleConnections()
	entered := make(chan struct{})
	handler := m.srv.Config.Handler
	m.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		handler.ServeHTTP(w, r)
	})
	finished := make(chan struct{})
	req := listenRequest(t, ctx, m.URL)
	go func() {
		defer close(finished)
		resp, err := (&http.Client{Transport: tr}).Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not reach server")
	}
	watchdog := time.AfterFunc(3*time.Second, cancel)
	defer watchdog.Stop()
	m.Close()
	if ctx.Err() != nil {
		t.Fatal("Close waited for the stalled client to cancel")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("stalled client did not disconnect")
	}
}

func listenRequest(t *testing.T, ctx context.Context, url string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": mcp.MethodSubscriptionsListen,
		"params": map[string]any{
			"notifications": map[string]any{"resourceSubscriptions": []string{"beekeeper://feed"}},
			"_meta":         map[string]any{mcp.MetaKeyProtocolVersion: mcp.ProtocolVersion20260728, mcp.MetaKeyClientInfo: map[string]any{"name": "test", "version": "1"}, mcp.MetaKeyClientCapabilities: map[string]any{}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(mcp.HeaderProtocolVersion, mcp.ProtocolVersion20260728)
	req.Header.Set(mcp.HeaderMethod, string(mcp.MethodSubscriptionsListen))
	return req
}
