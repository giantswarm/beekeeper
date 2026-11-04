package centraltest

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// Keep the real session behavior except for its notification transport.
type queuedSession struct {
	mcpserver.ClientSession
	queue     chan mcp.JSONRPCNotification
	attempted chan struct{}
}

func (s *queuedSession) NotificationChannel() chan<- mcp.JSONRPCNotification {
	if s.attempted != nil {
		s.attempted <- struct{}{}
	}
	return s.queue
}

type acknowledgedWriter struct {
	*httptest.ResponseRecorder
	acknowledged chan struct{}
	once         sync.Once
}

func (w *acknowledgedWriter) Flush() {
	w.ResponseRecorder.Flush()
	if strings.Contains(w.Body.String(), string(mcp.MethodNotificationSubscriptionsAcknowledged)) {
		w.once.Do(func() { close(w.acknowledged) })
	}
}

func await(t *testing.T, done <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal(message)
	}
}

func TestUpdatedCanceledSubscriber(t *testing.T) {
	m := &Muster{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"subscriptions/listen","params":{"_meta":{"io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/protocolVersion":"`+mcp.LATEST_PROTOCOL_VERSION+`"},"notifications":{"resourceSubscriptions":["beekeeper://feed"]}}}`)).WithContext(ctx)
	req.Header.Set(mcp.HeaderProtocolVersion, mcp.LATEST_PROTOCOL_VERSION)
	req.Header.Set("Mcp-Method", "subscriptions/listen")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	w := &acknowledgedWriter{ResponseRecorder: httptest.NewRecorder(), acknowledged: make(chan struct{})}
	handlerDone := make(chan struct{})
	go func() { m.handler("", "beekeeper").ServeHTTP(w, req); close(handlerDone) }()
	select {
	case <-w.acknowledged:
	case <-handlerDone:
		t.Fatalf("subscription ended before acknowledgment: %d %s", w.Code, w.Body.String())
	case <-time.After(2 * time.Second):
		t.Fatal("subscription was not acknowledged")
	}

	session := &queuedSession{queue: make(chan mcp.JSONRPCNotification, 100), attempted: make(chan struct{}, 1)}
	for range cap(session.queue) {
		session.queue <- mcp.JSONRPCNotification{}
	}
	m.mu.Lock()
	if len(m.streams) != 1 {
		m.mu.Unlock()
		t.Fatal("expected one registered stream")
	}
	var subscribed *stream
	for st := range m.streams {
		subscribed = st
		session.ClientSession = st.session
		st.session = session
	}
	m.mu.Unlock()
	// Release the queue even on failure so the original deadlock can be tested.
	defer func() { cancel(); <-session.queue }()
	updatedDone := make(chan struct{})
	reached := -1
	go func() { reached = m.Updated("beekeeper://feed"); close(updatedDone) }()
	await(t, session.attempted, "Updated did not attempt delivery")
	unlocked := make(chan struct{})
	go func() { m.mu.Lock(); m.mu.Unlock(); close(unlocked) }()
	select {
	case <-unlocked:
	case <-time.After(2 * time.Second):
		t.Error("notification backpressure holds the stream-map mutex")
	}
	cancel()
	select {
	case <-updatedDone:
		if reached != 0 {
			t.Errorf("Updated reached %d subscribers, want 0", reached)
		}
	case <-time.After(2 * time.Second):
		t.Error("Updated did not return after the subscriber disconnected")
		return
	}
	await(t, handlerDone, "subscription handler did not return")
	// Cleanup is asynchronous; the map must eventually lose the real registration.
	deadline := time.After(2 * time.Second)
	for {
		m.mu.Lock()
		_, exists := m.streams[subscribed]
		m.mu.Unlock()
		if !exists {
			break
		}
		select {
		case <-deadline:
			t.Fatal("disconnected stream was not removed")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestUpdatedDelivery(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "active"
		if canceled {
			name = "canceled"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if canceled {
				cancel()
			}
			session := &queuedSession{queue: make(chan mcp.JSONRPCNotification, 1)}
			st := &stream{ctx: ctx, session: session, id: 7, uris: []string{"beekeeper://feed"}}
			m := &Muster{streams: map[*stream]struct{}{st: {}}}
			if got := m.Updated("beekeeper://other"); got != 0 {
				t.Fatalf("unsubscribed URI reached %d streams", got)
			}
			want := 1
			if canceled {
				want = 0
			}
			if got := m.Updated("beekeeper://feed"); got != want {
				t.Fatalf("Updated = %d, want %d", got, want)
			}
			if len(session.queue) != want {
				t.Fatalf("queued %d notifications, want %d", len(session.queue), want)
			}
			if !canceled {
				notification := <-session.queue
				if notification.Method != mcp.MethodNotificationResourceUpdated || notification.Params.Meta[mcp.MetaKeySubscriptionID] != 7 || notification.Params.AdditionalFields["uri"] != "beekeeper://feed" {
					t.Fatalf("unexpected notification: %+v", notification)
				}
			}
		})
	}
}
