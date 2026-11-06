package central_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/beekeeper/internal/central"
	"github.com/giantswarm/beekeeper/internal/config"
)

func TestListenHandshakeTimeout(t *testing.T) {
	for _, flushHeaders := range []bool{false, true} {
		name := "headers"
		if flushHeaders {
			name = "acknowledgment"
		}
		t.Run(name, func(t *testing.T) {
			s := mcpserver.NewMCPServer("muster", "test", mcpserver.WithResourceCapabilities(true, false))
			h := mcpserver.NewStreamableHTTPServer(s, mcpserver.WithStateLess(true))
			var subscribed atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get(mcp.HeaderMethod) == string(mcp.MethodSubscriptionsListen) {
					_, _ = io.Copy(io.Discard, r.Body)
					subscribed.Store(true)
					if flushHeaders {
						w.Header().Set("Content-Type", "text/event-stream")
						w.(http.Flusher).Flush()
					}
					<-r.Context().Done()
					return
				}
				h.ServeHTTP(w, r)
			}))
			t.Cleanup(srv.Close)
			endpoint, _ := json.Marshal(map[string]string{"endpoint": srv.URL + "/mcp"})
			run := func(_ context.Context, args ...string) ([]byte, error) {
				if args[0] == "context" {
					return endpoint, nil
				}
				return []byte("synthetic-token"), nil
			}
			c := central.New(config.Central{Context: labContext, Timeout: config.Duration{Duration: time.Second}}, run)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err := c.Listen(ctx, []string{mailboxURI}, func() { t.Error("unexpected acknowledgment") }, func(string) {})
			if !subscribed.Load() {
				t.Fatal("fixture did not reach subscriptions/listen")
			}
			var u *central.Unreachable
			if !errors.As(err, &u) || !strings.Contains(u.Reason, "timed out (deadline 1s)") {
				t.Fatalf("Listen = %v (%T), want a subscription handshake timeout before the parent ends", err, err)
			}
		})
	}
}

func TestListenOutlivesHandshakeTimeout(t *testing.T) {
	updates := make(chan struct{})
	c, _ := clientOf(t, subscriber(t, updates), "beekeeper", time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{}, 1)
	updated := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.Listen(ctx, []string{mailboxURI}, func() { ready <- struct{}{} }, func(uri string) { updated <- uri })
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Listen ended before acknowledgment: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no subscription acknowledgment")
	}
	select {
	case err := <-done:
		t.Fatalf("acknowledged stream ended at the handshake deadline: %v", err)
	case <-time.After(2 * time.Second):
	}
	close(updates)
	select {
	case uri := <-updated:
		if uri != mailboxURI {
			t.Errorf("updated %q, want %s", uri, mailboxURI)
		}
	case err := <-done:
		t.Fatalf("Listen ended before the later update: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no update after the handshake deadline")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Listen after caller cancellation = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("caller cancellation did not stop Listen")
	}
}
