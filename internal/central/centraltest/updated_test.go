package centraltest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

const testFeedURI = "beekeeper://feed"

type acknowledgedWriter struct {
	*httptest.ResponseRecorder
	ready chan struct{}
	once  sync.Once
}

func (w *acknowledgedWriter) Flush() {
	w.ResponseRecorder.Flush()
	w.once.Do(func() { close(w.ready) })
}

// blockedSession models a subscriber whose bounded notification queue is
// full and whose transport no longer consumes it.
type blockedSession struct {
	mcpserver.ClientSession
	notifications chan mcp.JSONRPCNotification
	sending       chan struct{}
	once          sync.Once
}

func (s *blockedSession) NotificationChannel() chan<- mcp.JSONRPCNotification {
	s.once.Do(func() { close(s.sending) })
	return s.notifications
}

func TestUpdatedCanceledSubscriber(t *testing.T) {
	m := &Muster{}
	h := m.handler("http://backend.invalid/mcp", "beekeeper")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":"subscriptions/listen","params":{"_meta":{%q:%q,%q:{}},"notifications":{"resourceSubscriptions":[%q]}}}`, mcp.MetaKeyProtocolVersion, mcp.LATEST_PROTOCOL_VERSION, mcp.MetaKeyClientCapabilities, testFeedURI)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", mcp.LATEST_PROTOCOL_VERSION)
	req.Header.Set("Mcp-Method", "subscriptions/listen")
	w := &acknowledgedWriter{ResponseRecorder: httptest.NewRecorder(), ready: make(chan struct{})}
	served := make(chan struct{})
	go func() {
		h.ServeHTTP(w, req)
		close(served)
	}()
	select {
	case <-w.ready:
	case <-time.After(time.Second):
		t.Fatal("subscription was not acknowledged")
	}
	if got := m.Updated("beekeeper://other"); got != 0 {
		t.Fatalf("unsubscribed URI reached %d streams", got)
	}

	// Keep the actual request lifecycle and replace only the transport
	// queue. This makes backpressure deterministic without filling a socket.
	blocked := &blockedSession{notifications: make(chan mcp.JSONRPCNotification, 1), sending: make(chan struct{})}
	blocked.notifications <- mcp.JSONRPCNotification{}
	m.mu.Lock()
	for st := range m.streams {
		blocked.ClientSession = st.session
		st.session = blocked
	}
	m.mu.Unlock()
	sent := make(chan int, 1)
	go func() { sent <- m.Updated(testFeedURI) }()
	select {
	case <-blocked.sending:
	case <-time.After(time.Second):
		t.Fatal("update did not reach the subscribed stream")
	}

	// Backpressure on one stream must not lock out subscription registration
	// or cleanup, which both need the stream-map mutex.
	unlocked := make(chan struct{})
	go func() {
		m.mu.Lock()
		n := len(m.streams)
		m.mu.Unlock()
		if n != 1 {
			t.Errorf("registered streams = %d, want the active subscriber", n)
		}
		close(unlocked)
	}()
	select {
	case <-unlocked:
	case <-time.After(time.Second):
		t.Error("notification backpressure holds the stream-map mutex")
	}
	cancel()
	select {
	case n := <-sent:
		if n != 0 {
			t.Errorf("update reached %d canceled streams, want 0", n)
		}
	case <-time.After(time.Second):
		// Release the old blocking send so a regression cannot leak it.
		<-blocked.notifications
		<-sent
		t.Error("Updated did not return after the subscriber disconnected")
	}
	<-served
	<-unlocked
}

func TestUpdatedTargetsLiveSubscribers(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	m := &Muster{streams: map[*stream]struct{}{}}
	for i, tc := range []struct {
		ctx context.Context
		uri string
	}{
		{t.Context(), testFeedURI},
		{t.Context(), testFeedURI},
		{t.Context(), "beekeeper://other"},
		{canceled, testFeedURI},
	} {
		session := &blockedSession{notifications: make(chan mcp.JSONRPCNotification, 1), sending: make(chan struct{})}
		m.streams[&stream{ctx: tc.ctx, session: session, id: i, uris: []string{tc.uri}}] = struct{}{}
	}
	if got := m.Updated(testFeedURI); got != 2 {
		t.Fatalf("update reached %d streams, want the two live subscribers", got)
	}
	for st := range m.streams {
		q := st.session.(*blockedSession).notifications
		if st.id.(int) >= 2 {
			if len(q) != 0 {
				t.Error("update reached an unrelated or canceled subscription")
			}
			continue
		}
		n := <-q
		if n.Method != mcp.MethodNotificationResourceUpdated || n.Params.AdditionalFields["uri"] != testFeedURI || n.Params.Meta[mcp.MetaKeySubscriptionID] != st.id {
			t.Errorf("update does not name its resource and subscription: %+v", n)
		}
	}
}
