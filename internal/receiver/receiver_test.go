package receiver_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/inbox"
	"github.com/giantswarm/beekeeper/internal/metrics"
	"github.com/giantswarm/beekeeper/internal/receiver"
)

var (
	secret = []byte("webhook-secret")
	t0     = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
)

const board = "PVT_giantswarm_273"

// Webhook events of the test deliveries, and the event label of those the receiver doesn't subscribe to.
const (
	eventCheckRun       = "check_run"
	eventPing           = "ping"
	eventProjectsV2Item = "projects_v2_item"
	eventOther          = "other"
)

// store is an in-memory inbox.Store.
type store struct {
	mu   sync.Mutex
	rows map[string]inbox.Delivery
	err  error
	// gate, when set, holds the Put of every delivery whose id starts with "slow" until it is closed;
	// held reports each one as it starts waiting.
	gate chan struct{}
	held chan string
}

func (s *store) Put(_ context.Context, d inbox.Delivery) (bool, error) {
	if s.gate != nil && strings.HasPrefix(d.ID, "slow") {
		s.held <- d.ID
		<-s.gate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return false, s.err
	}
	if _, dup := s.rows[d.ID]; dup {
		return true, nil
	}
	s.rows[d.ID] = d
	return false, nil
}

type fixture struct {
	h          http.Handler
	store      *store
	deliveries *prometheus.CounterVec
}

func newFixture(maxBody int64) *fixture {
	f := &fixture{
		store: &store{rows: map[string]inbox.Delivery{}},
		deliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "engine_webhook_deliveries_total",
		}, []string{"event", "outcome"}),
	}
	f.h = receiver.New(receiver.Config{
		Path:           "/github/webhook",
		MaxBodyBytes:   maxBody,
		Secret:         secret,
		Store:          f.store,
		Deliveries:     f.deliveries,
		Clock:          clock.NewFake(t0),
		ProjectNodeIDs: []string{board},
	})
	return f
}

type request struct {
	method, path, event, delivery string
	body                          []byte
	signature                     string // "valid" (default), "invalid", "missing", "malformed", "nohex"
	chunked                       bool   // no Content-Length
}

func (f *fixture) do(r request) *httptest.ResponseRecorder {
	if r.method == "" {
		r.method = http.MethodPost
	}
	if r.path == "" {
		r.path = "/github/webhook"
	}
	var body io.Reader = bytes.NewReader(r.body)
	if r.chunked {
		body = io.MultiReader(bytes.NewReader(r.body)) // hides the length
	}
	req := httptest.NewRequest(r.method, r.path, body)
	if r.chunked {
		req.ContentLength = -1
	}
	if r.event != "" {
		req.Header.Set("X-GitHub-Event", r.event)
	}
	if r.delivery != "" {
		req.Header.Set("X-GitHub-Delivery", r.delivery)
	}
	switch r.signature {
	case "", "valid":
		req.Header.Set("X-Hub-Signature-256", receiver.Sign(secret, r.body))
	case "invalid":
		req.Header.Set("X-Hub-Signature-256", receiver.Sign([]byte("not-the-secret"), r.body))
	case "malformed":
		req.Header.Set("X-Hub-Signature-256", strings.TrimPrefix(receiver.Sign(secret, r.body), "sha256="))
	case "nohex":
		req.Header.Set("X-Hub-Signature-256", "sha256=zz")
	case "sha1":
		req.Header.Set("X-Hub-Signature-256", "sha1="+strings.TrimPrefix(receiver.Sign(secret, r.body), "sha256="))
	case "missing":
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) count(event, outcome string) float64 {
	return testutil.ToFloat64(f.deliveries.WithLabelValues(event, outcome))
}

var (
	checkRun = []byte(`{"action":"completed","check_run":{"name":"ci/circleci: go-test","head_sha":"a1","status":"completed","conclusion":"success"},"repository":{"full_name":"giantswarm/klaus"},"installation":{"id":200}}`)
	ping     = []byte(`{"zen":"hello","hook_id":1,"hook":{"type":"App","app_id":100}}`)
)

func projectItem(project string) []byte {
	return []byte(fmt.Sprintf(`{"action":"edited","projects_v2_item":{"node_id":"PVTI_x","project_node_id":%q,"content_node_id":"I_x","content_type":"Issue"},"changes":{"field_value":{"field_name":"Status","to":{"name":"Up Next ➡️"}}},"organization":{"login":"giantswarm"}}`, project))
}

// padded returns a valid JSON body of exactly n bytes.
func padded(n int) []byte {
	head := `{"action":"completed","pad":"`
	tail := `"}`
	return []byte(head + strings.Repeat("x", n-len(head)-len(tail)) + tail)
}

func TestOutcomes(t *testing.T) {
	t.Parallel()
	const limit = 4096
	cases := []struct {
		name    string
		req     request
		code    int
		event   string // label
		outcome string // "" means not counted
		stored  bool
	}{
		{"valid signature accepted", request{event: eventCheckRun, delivery: "d1", body: checkRun}, 202, eventCheckRun, metrics.DeliveryAccepted, true},
		{"invalid signature", request{event: eventCheckRun, delivery: "d1", body: checkRun, signature: "invalid"}, 401, eventCheckRun, metrics.DeliveryBadSignature, false},
		{"signature without sha256= prefix", request{event: eventCheckRun, delivery: "d1", body: checkRun, signature: "malformed"}, 401, eventCheckRun, metrics.DeliveryBadSignature, false},
		{"signature not hex", request{event: eventCheckRun, delivery: "d1", body: checkRun, signature: "nohex"}, 401, eventCheckRun, metrics.DeliveryBadSignature, false},
		{"sha1 signature", request{event: eventCheckRun, delivery: "d1", body: checkRun, signature: "sha1"}, 401, eventCheckRun, metrics.DeliveryBadSignature, false},
		{"missing signature", request{event: "issues", delivery: "d1", body: []byte(`{"action":"edited","issue":{"number":101}}`), signature: "missing"}, 401, "issues", metrics.DeliveryMissingSignature, false},
		{"one byte over the limit", request{event: eventCheckRun, delivery: "d1", body: padded(limit + 1)}, 413, eventCheckRun, metrics.DeliveryTooLarge, false},
		{"one byte over the limit, no Content-Length", request{event: eventCheckRun, delivery: "d1", body: padded(limit + 1), chunked: true}, 413, eventCheckRun, metrics.DeliveryTooLarge, false},
		{"exactly the limit", request{event: eventCheckRun, delivery: "d1", body: padded(limit)}, 202, eventCheckRun, metrics.DeliveryAccepted, true},
		{"exactly the limit, no Content-Length", request{event: eventCheckRun, delivery: "d1", body: padded(limit), chunked: true}, 202, eventCheckRun, metrics.DeliveryAccepted, true},
		{"ping acknowledged", request{event: eventPing, delivery: "d1", body: ping}, 200, eventPing, metrics.DeliveryIgnored, false},
		{"unknown event ignored as other", request{event: "deployment", delivery: "d1", body: []byte(`{"action":"created","deployment":{"sha":"a1"}}`)}, 200, eventOther, metrics.DeliveryIgnored, false},
		{"no event header", request{delivery: "d1", body: checkRun}, 200, eventOther, metrics.DeliveryIgnored, false},
		{"other project's item ignored", request{event: eventProjectsV2Item, delivery: "d1", body: projectItem("PVT_giantswarm_999")}, 200, eventProjectsV2Item, metrics.DeliveryIgnored, false},
		{"board's item accepted", request{event: eventProjectsV2Item, delivery: "d1", body: projectItem(board)}, 202, eventProjectsV2Item, metrics.DeliveryAccepted, true},
		{"no delivery id", request{event: eventCheckRun, body: checkRun}, 400, eventCheckRun, metrics.DeliveryIgnored, false},
		{"not JSON", request{event: eventCheckRun, delivery: "d1", body: []byte(`action=completed`)}, 400, eventCheckRun, metrics.DeliveryIgnored, false},
		{"GET on the path", request{method: http.MethodGet, event: eventPing, delivery: "d1", body: ping}, 405, "", "", false},
		{"PUT on the path", request{method: http.MethodPut, event: eventCheckRun, delivery: "d1", body: checkRun}, 405, "", "", false},
		{"trailing slash", request{path: "/github/webhook/", event: eventPing, delivery: "d1", body: ping}, 404, "", "", false},
		{"root", request{path: "/", event: eventPing, delivery: "d1", body: ping}, 404, "", "", false},
		{"other path", request{path: "/github/webhook2", event: eventCheckRun, delivery: "d1", body: checkRun}, 404, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(limit)
			rec := f.do(c.req)
			if rec.Code != c.code {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, c.code, rec.Body)
			}
			if got := testutil.CollectAndCount(f.deliveries); (c.outcome == "") != (got == 0) {
				t.Fatalf("%d series counted, want outcome %q", got, c.outcome)
			}
			if c.outcome != "" {
				if got := f.count(c.event, c.outcome); got != 1 {
					t.Fatalf("engine_webhook_deliveries_total{event=%q,outcome=%q} = %v, want 1", c.event, c.outcome, got)
				}
			}
			if stored := len(f.store.rows) == 1; stored != c.stored {
				t.Fatalf("stored = %v, want %v", stored, c.stored)
			}
			if c.stored {
				d := f.store.rows["d1"]
				if d.Event != c.req.event || d.Action == "" || !d.ReceivedAt.Equal(t0) || !bytes.Equal(d.Payload, c.req.body) {
					t.Fatalf("stored %+v", d)
				}
			}
		})
	}
}

func TestReplayedDeliveryIsDuplicate(t *testing.T) {
	t.Parallel()
	f := newFixture(0)
	first := f.do(request{event: eventCheckRun, delivery: "7c1e9a40-0001", body: checkRun})
	again := f.do(request{event: eventCheckRun, delivery: "7c1e9a40-0001", body: checkRun})
	if first.Code != 202 || again.Code != 200 {
		t.Fatalf("status = %d then %d, want 202 then 200", first.Code, again.Code)
	}
	if f.count(eventCheckRun, metrics.DeliveryAccepted) != 1 || f.count(eventCheckRun, metrics.DeliveryDuplicate) != 1 {
		t.Fatalf("accepted %v, duplicate %v", f.count(eventCheckRun, metrics.DeliveryAccepted), f.count(eventCheckRun, metrics.DeliveryDuplicate))
	}
	if d := f.store.rows["7c1e9a40-0001"]; d.Action != "completed" || d.Event != eventCheckRun {
		t.Fatalf("stored %+v", d)
	}
}

func TestStoreFailureIs503(t *testing.T) {
	t.Parallel()
	f := newFixture(0)
	f.store.err = errors.New("connection refused")
	rec := f.do(request{event: eventCheckRun, delivery: "d1", body: checkRun})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if f.count(eventCheckRun, metrics.DeliveryUnavailable) != 1 {
		t.Fatal("not counted unavailable")
	}
}

// The default limit is 10 MiB, inclusive (webhook-oversize-rejected).
func TestDefaultLimitIsTenMiBInclusive(t *testing.T) {
	t.Parallel()
	f := newFixture(0)
	if rec := f.do(request{event: eventCheckRun, delivery: "big", body: padded(10<<20 + 1)}); rec.Code != 413 {
		t.Fatalf("10 MiB + 1: status %d, want 413", rec.Code)
	}
	if rec := f.do(request{event: eventCheckRun, delivery: "big", body: padded(10 << 20)}); rec.Code != 202 {
		t.Fatalf("10 MiB: status %d, want 202", rec.Code)
	}
}

func TestEveryProjectAcceptedWithoutBoards(t *testing.T) {
	t.Parallel()
	st := &store{rows: map[string]inbox.Delivery{}}
	h := receiver.New(receiver.Config{Secret: secret, Store: st, Clock: clock.NewFake(t0)})
	body := projectItem("PVT_giantswarm_999")
	req := httptest.NewRequest(http.MethodPost, receiver.DefaultPath, bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", eventProjectsV2Item)
	req.Header.Set("X-GitHub-Delivery", "d1")
	req.Header.Set("X-Hub-Signature-256", receiver.Sign(secret, body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 202 || len(st.rows) != 1 {
		t.Fatalf("status %d, stored %d", rec.Code, len(st.rows))
	}
}

func TestEventLabel(t *testing.T) {
	t.Parallel()
	for event, want := range map[string]string{
		eventCheckRun: eventCheckRun, eventPing: eventPing, "push": "push", "installation_repositories": "installation_repositories",
		"deployment": eventOther, "": eventOther, "installation": eventOther, "projects_v2": eventOther,
	} {
		if got := receiver.EventLabel(event); got != want {
			t.Errorf("EventLabel(%q) = %q, want %q", event, got, want)
		}
	}
}

func TestConfigValidated(t *testing.T) {
	t.Parallel()
	ok := receiver.Config{Secret: secret, Store: &store{}, Clock: clock.NewFake(t0)}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]receiver.Config{
		"no secret": {Store: &store{}, Clock: clock.NewFake(t0)},
		"no store":  {Secret: secret, Clock: clock.NewFake(t0)},
		"no clock":  {Secret: secret, Store: &store{}},
	} {
		if c.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// unread is a body that records whether anyone read it.
type unread struct {
	r    io.Reader
	read atomic.Bool
}

func (u *unread) Read(p []byte) (int, error) { u.read.Store(true); return u.r.Read(p) }

// The receiver holds at most MaxInFlightBytes of bodies at once, by their declared size: a request
// that doesn't fit is answered 503 at once, before its body is read, and counted unavailable.
func TestInFlightBodiesAreBounded(t *testing.T) {
	t.Parallel()
	const limit = 4096
	st := &store{rows: map[string]inbox.Delivery{}, gate: make(chan struct{}), held: make(chan string, 2)}
	f := &fixture{store: st, deliveries: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "engine_webhook_deliveries_total"}, []string{"event", "outcome"})}
	f.h = receiver.New(receiver.Config{MaxBodyBytes: limit, MaxInFlightBytes: 2 * limit, Secret: secret, Store: st,
		Deliveries: f.deliveries, Clock: clock.NewFake(t0)})
	post := func(id string, body []byte, chunked bool) (int, bool) {
		u := &unread{r: bytes.NewReader(body)}
		req := httptest.NewRequest(http.MethodPost, receiver.DefaultPath, u)
		req.ContentLength = int64(len(body))
		if chunked {
			req.ContentLength = -1
		}
		req.Header.Set("X-GitHub-Event", eventCheckRun)
		req.Header.Set("X-GitHub-Delivery", id)
		req.Header.Set("X-Hub-Signature-256", receiver.Sign(secret, body))
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		return rec.Code, u.read.Load()
	}

	// Two deliveries are being stored, holding 3,000 + 4,096 of the 8,192 bytes.
	var slow sync.WaitGroup
	codes := make(chan int, 2)
	for _, d := range []struct {
		id   string
		size int
	}{{"slow-1", 3000}, {"slow-2", limit}} {
		slow.Add(1)
		go func() {
			defer slow.Done()
			code, _ := post(d.id, padded(d.size), false)
			codes <- code
		}()
		<-st.held
	}

	// 1,096 bytes are left.
	if code, read := post("too-large", padded(2000), false); code != http.StatusServiceUnavailable || read {
		t.Fatalf("a delivery that doesn't fit: %d, body read %v", code, read)
	}
	// Without a Content-Length a body might be MaxBodyBytes long.
	if code, read := post("chunked", padded(100), true); code != http.StatusServiceUnavailable || read {
		t.Fatalf("a delivery without a length: %d, body read %v", code, read)
	}
	if code, read := post("fits", padded(1000), false); code != http.StatusAccepted || !read {
		t.Fatalf("a delivery that fits: %d, body read %v", code, read)
	}

	close(st.gate)
	slow.Wait()
	for range 2 {
		if code := <-codes; code != http.StatusAccepted {
			t.Fatalf("a held delivery: %d", code)
		}
	}
	// Stored, they gave their bytes back.
	if code, _ := post("after", padded(limit), true); code != http.StatusAccepted {
		t.Fatalf("after the held deliveries: %d", code)
	}
	if f.count(eventCheckRun, metrics.DeliveryUnavailable) != 2 || f.count(eventCheckRun, metrics.DeliveryAccepted) != 4 {
		t.Fatalf("unavailable %v, accepted %v", f.count(eventCheckRun, metrics.DeliveryUnavailable), f.count(eventCheckRun, metrics.DeliveryAccepted))
	}
	for _, id := range []string{"too-large", "chunked"} {
		if _, ok := st.rows[id]; ok {
			t.Fatalf("%s stored", id)
		}
	}
}

// The budget is never smaller than one body of the largest size, and defaults to four of them.
func TestInFlightBudgetDefaults(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		inFlight int64
		fits     int // bodies of MaxBodyBytes held at once
	}{
		"default":            {0, receiver.DefaultInFlightBodies},
		"below one body":     {100, 1},
		"exactly two bodies": {2 * 1024, 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			const limit = 1024
			st := &store{rows: map[string]inbox.Delivery{}, gate: make(chan struct{}), held: make(chan string, c.fits)}
			h := receiver.New(receiver.Config{MaxBodyBytes: limit, MaxInFlightBytes: c.inFlight, Secret: secret, Store: st, Clock: clock.NewFake(t0)})
			post := func(id string) int {
				body := padded(limit)
				req := httptest.NewRequest(http.MethodPost, receiver.DefaultPath, bytes.NewReader(body))
				req.Header.Set("X-GitHub-Event", eventCheckRun)
				req.Header.Set("X-GitHub-Delivery", id)
				req.Header.Set("X-Hub-Signature-256", receiver.Sign(secret, body))
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				return rec.Code
			}
			var wg sync.WaitGroup
			for i := range c.fits {
				wg.Add(1)
				go func() { defer wg.Done(); post(fmt.Sprintf("slow-%d", i)) }()
				<-st.held
			}
			if code := post("one-more"); code != http.StatusServiceUnavailable {
				t.Errorf("one body more than %d: %d", c.fits, code)
			}
			close(st.gate)
			wg.Wait()
		})
	}
}
