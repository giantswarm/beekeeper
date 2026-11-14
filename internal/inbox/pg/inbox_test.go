package inboxpg_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/inbox"
	inboxpg "github.com/giantswarm/beekeeper/internal/inbox/pg"
	"github.com/giantswarm/beekeeper/internal/settle"
	"github.com/giantswarm/beekeeper/internal/testing/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var t0 = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

// actionCompleted is the action of the check_run deliveries the tests store.
const actionCompleted = "completed"

func delivery(id string) inbox.Delivery {
	return inbox.Delivery{
		ID: id, Event: "check_run", Action: actionCompleted, ReceivedAt: t0,
		Payload: []byte(`{"action":"completed","check_run":{"head_sha":"a1"}}`),
	}
}

type env struct {
	db      *pgtest.DB
	clock   *clock.Fake
	tracker *settle.Tracker
	store   *inboxpg.Store
	drainer *inboxpg.Drainer

	mu      sync.Mutex
	handled []string
	fail    map[string]int // delivery id → failures left
}

func newEnv(t *testing.T, opts ...inboxpg.Option) *env {
	t.Helper()
	e := &env{db: pgtest.NewDB(t), clock: clock.NewFake(t0), tracker: settle.New(), fail: map[string]int{}}
	e.store = inboxpg.NewStore(e.db.Pool())
	// An hour of real time: the tests prove NOTIFY and the probe wake the drainer, not a poll.
	opts = append([]inboxpg.Option{inboxpg.WithRetryInterval(time.Hour)}, opts...)
	e.drainer = inboxpg.NewDrainer(e.db.Pool(), e.clock, e.tracker, opts...)
	t.Cleanup(e.drainer.Close)
	return e
}

func (e *env) handler(_ context.Context, d inbox.Delivery) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fail[d.ID] > 0 {
		e.fail[d.ID]--
		return errors.New("handler failed")
	}
	e.handled = append(e.handled, d.ID)
	return nil
}

func (e *env) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.drainer.Run(ctx, e.handler) }()
	// The probe is idle until Run runs (a standby has nothing to drain), so wait for it.
	for !e.drainer.Running() {
		time.Sleep(100 * time.Microsecond)
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
}

func (e *env) put(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := e.store.Put(context.Background(), delivery(id)); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *env) settle(t *testing.T) {
	t.Helper()
	if err := e.tracker.Wait(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

func (e *env) handledIDs() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.handled, ",")
}

func (e *env) unprocessed(t *testing.T) int {
	t.Helper()
	n, err := e.drainer.Unprocessed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPutReportsDuplicates(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	for i, want := range []bool{false, true, true} {
		dup, err := e.store.Put(ctx, delivery("7c1e9a40-0001"))
		if err != nil {
			t.Fatal(err)
		}
		if dup != want {
			t.Fatalf("put %d: duplicate = %v, want %v", i, dup, want)
		}
	}
	if n := e.unprocessed(t); n != 1 {
		t.Fatalf("unprocessed = %d, want 1", n)
	}
}

// Deliveries stored while no drainer runs (no leader) are drained by the next, in arrival order, and
// marked processed with the engine clock's time.
func TestDrainInArrivalOrder(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	// Arrival order isn't the ids' order, and they all arrive at the same engine instant.
	e.put(t, "c", "a", "e", "b", "d")
	idle, _ := e.tracker.Idle()
	if !idle {
		t.Fatal("not running, yet busy: a standby has nothing to drain")
	}
	e.clock.Advance(time.Minute)
	e.run(t)
	e.settle(t)
	if got := e.handledIDs(); got != "c,a,e,b,d" {
		t.Fatalf("handled %s, want c,a,e,b,d", got)
	}
	if n := e.unprocessed(t); n != 0 {
		t.Fatalf("unprocessed = %d", n)
	}
	var at time.Time
	if err := e.db.Pool().QueryRow(context.Background(), "SELECT processed_at FROM inbox WHERE delivery_id = 'a'").Scan(&at); err != nil {
		t.Fatal(err)
	}
	if !at.Equal(t0.Add(time.Minute)) {
		t.Fatalf("processed_at = %s, want the engine clock's %s", at, t0.Add(time.Minute))
	}
}

func TestDeliveryRoundTrip(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var got inbox.Delivery
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.drainer.Run(ctx, func(_ context.Context, d inbox.Delivery) error {
			mu.Lock()
			got = d
			mu.Unlock()
			return nil
		})
	}()
	defer func() { cancel(); <-done }()
	for !e.drainer.Running() {
		time.Sleep(100 * time.Microsecond)
	}
	e.put(t, "x")
	e.settle(t)
	mu.Lock()
	defer mu.Unlock()
	if got.ID != "x" || got.Event != "check_run" || got.Action != actionCompleted || !got.ReceivedAt.Equal(t0) {
		t.Fatalf("delivery = %+v", got)
	}
	var payload map[string]any
	if err := json.Unmarshal(got.Payload, &payload); err != nil || payload["action"] != actionCompleted {
		t.Fatalf("payload = %s (%v)", got.Payload, err)
	}
}

// A delivery stored while the drainer runs wakes it through NOTIFY (no polling: the retry interval is an
// hour), and the settle probe is busy until it is handled.
func TestNotifyWakesDrainer(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run(t)
	e.settle(t)
	for i := range 3 {
		id := fmt.Sprintf("d%d", i)
		e.put(t, id)
		e.settle(t)
		if got := e.handledIDs(); !strings.HasSuffix(got, id) {
			t.Fatalf("after storing %s and settling, handled = %s", id, got)
		}
	}
	// Without the tracker's help: NOTIFY alone wakes it.
	e.put(t, "d3")
	deadline := time.Now().Add(5 * time.Second)
	for !strings.HasSuffix(e.handledIDs(), "d3") {
		if time.Now().After(deadline) {
			t.Fatal("NOTIFY didn't wake the drainer")
		}
		time.Sleep(time.Millisecond)
	}
}

// A failing handler leaves its delivery unprocessed; later deliveries go on, and it is retried after a
// backoff on the engine clock. While it waits for that, the probe is idle.
func TestHandlerErrorLeavesRow(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.fail["b"] = 2
	e.put(t, "a", "b", "c")
	e.run(t)
	e.settle(t)
	const handled = "a,c" // b's handler fails
	if got := e.handledIDs(); got != handled {
		t.Fatalf("handled %s, want a,c", got)
	}
	if n := e.unprocessed(t); n != 1 {
		t.Fatalf("unprocessed = %d, want 1 (b)", n)
	}

	e.clock.Advance(10 * time.Second) // first retry fails again
	e.settle(t)
	if got := e.handledIDs(); got != handled || e.unprocessed(t) != 1 {
		t.Fatalf("after the first retry: handled %s", got)
	}
	e.clock.Advance(10 * time.Second) // the backoff doubled: not yet
	e.settle(t)
	if got := e.handledIDs(); got != handled {
		t.Fatalf("retried before the doubled backoff: %s", got)
	}
	e.clock.Advance(10 * time.Second)
	e.settle(t)
	if got := e.handledIDs(); got != "a,c,b" || e.unprocessed(t) != 0 {
		t.Fatalf("after the second retry: handled %s, unprocessed %d", got, e.unprocessed(t))
	}
}

func TestProcessedRowsDeletedAfterRetention(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run(t)
	e.put(t, "old")
	e.settle(t)
	count := func() int {
		var n int
		if err := e.db.Pool().QueryRow(context.Background(), "SELECT count(*) FROM inbox").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	e.clock.Advance(71 * time.Hour)
	e.put(t, "newer")
	e.settle(t)
	if n := count(); n != 2 {
		t.Fatalf("rows after 71h = %d, want 2", n)
	}
	e.clock.Advance(2 * time.Hour)
	e.put(t, "newest")
	e.settle(t)
	if n := count(); n != 2 {
		t.Fatalf("rows after 73h = %d, want 2 (the oldest deleted)", n)
	}
	var old int
	_ = e.db.Pool().QueryRow(context.Background(), "SELECT count(*) FROM inbox WHERE delivery_id = 'old'").Scan(&old)
	if old != 0 {
		t.Fatal("delivery processed 73h ago still kept")
	}
}

// The drainer survives Postgres going away and drains what was stored once it is back.
func TestDrainerReconnects(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run(t)
	e.put(t, "a")
	e.settle(t)
	if err := e.db.Down(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Put(context.Background(), delivery("lost")); err == nil {
		t.Fatal("stored while Postgres is down")
	}
	e.settle(t) // idle: nothing can be drained
	if err := e.db.Up(); err != nil {
		t.Fatal(err)
	}
	e.put(t, "b")
	e.settle(t) // the probe sees b and wakes the drainer at once, not after the hour
	if got := e.handledIDs(); got != "a,b" {
		t.Fatalf("handled %s, want a,b", got)
	}
}

func TestRunTwiceRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run(t)
	e.put(t, "a")
	e.settle(t)
	if e.handledIDs() != "a" {
		t.Fatal("not running")
	}
	if err := e.drainer.Run(context.Background(), e.handler); err == nil {
		t.Fatal("second Run not refused")
	}
}

// Payloads are stored as the bytes GitHub sent: a NUL escape (which jsonb refuses), the key order and
// the spacing come back as they were (review 1, finding 6a).
func TestPayloadIsStoredAsSent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	payload := []byte(`{"zeta": 1, "action":"edited","issue":{"number":101,"body":"a\u0000b"}}`)
	d := delivery("nul")
	d.Event, d.Action, d.Payload = "issues", "edited", payload
	if dup, err := e.store.Put(context.Background(), d); err != nil || dup {
		t.Fatalf("Put: duplicate %v, %v", dup, err)
	}
	var got []byte
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.drainer.Run(ctx, func(_ context.Context, d inbox.Delivery) error {
			mu.Lock()
			got = d.Payload
			mu.Unlock()
			return nil
		})
	}()
	defer func() { cancel(); <-done }()
	for !e.drainer.Running() {
		time.Sleep(100 * time.Microsecond)
	}
	e.settle(t)
	mu.Lock()
	defer mu.Unlock()
	if string(got) != string(payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
	var p struct {
		Issue struct {
			Body string `json:"body"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(got, &p); err != nil || p.Issue.Body != "a\x00b" {
		t.Fatalf("decoded body %q (%v)", p.Issue.Body, err)
	}
}

// A delivery the handler holds stays unprocessed, but out of the drain and idle, until it is
// acknowledged (also when the Ack comes before the handler returns); a new Run hands it out again.
func TestHeldUntilAcknowledged(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var mu sync.Mutex
	calls := map[string]int{}
	ackEarly := map[string]bool{"early": true}
	h := func(_ context.Context, d inbox.Delivery) error {
		mu.Lock()
		calls[d.ID]++
		mu.Unlock()
		if ackEarly[d.ID] {
			e.drainer.Ack(d.ID)
		}
		return inbox.ErrHeld
	}
	e.put(t, "held", "early", "lost")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.drainer.Run(ctx, h) }()
	for !e.drainer.Running() {
		time.Sleep(100 * time.Microsecond)
	}
	e.settle(t)
	if n := e.unprocessed(t); n != 2 {
		t.Fatalf("unprocessed = %d, want 2 (held, lost)", n)
	}
	e.drainer.Ack("held")
	e.settle(t)
	if n := e.unprocessed(t); n != 1 {
		t.Fatalf("after the ack: unprocessed = %d, want 1 (lost)", n)
	}
	mu.Lock()
	if calls["held"] != 1 || calls["early"] != 1 || calls["lost"] != 1 {
		t.Fatalf("handled %v, want each once", calls)
	}
	mu.Unlock()

	// The next leader's Run hands the unacknowledged delivery out again.
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	e.run(t)
	e.settle(t)
	if got := e.handledIDs(); got != "lost" || e.unprocessed(t) != 0 {
		t.Fatalf("next run handled %q, unprocessed %d", got, e.unprocessed(t))
	}
}
