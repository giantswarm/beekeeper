// Package receiver is the hardened webhook endpoint (plan §4, contract §1.3): it answers only
// POST <Path>, caps the body, bounds the bodies it holds at once, verifies X-Hub-Signature-256 in
// constant time, and stores each verified delivery of a subscribed event in the inbox (deduplicated by
// X-GitHub-Delivery) before it answers 2xx. It trusts nothing in the payload but its action (and, for
// projects_v2_item, which project it is about), and stores the payload as the bytes GitHub sent.
package receiver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/inbox"
	"github.com/giantswarm/beekeeper/internal/settle"
)

// Defaults.
const (
	DefaultPath         = "/github/webhook"
	DefaultMaxBodyBytes = 10 << 20 // 10 MiB, inclusive
	// DefaultInFlightBodies is how many bodies of the largest size the default in-flight budget holds.
	DefaultInFlightBodies = 4
	// DefaultStoreTimeout is GitHub's own delivery timeout: an answer after it is a failed delivery anyway.
	DefaultStoreTimeout = 10 * time.Second
)

// Outcomes, the outcome label of engine_webhook_deliveries_total.
const (
	OutcomeAccepted         = "accepted"
	OutcomeBadSignature     = "bad_signature"
	OutcomeMissingSignature = "missing_signature"
	OutcomeDuplicate        = "duplicate"
	OutcomeTooLarge         = "too_large"
	OutcomeIgnored          = "ignored"
	OutcomeUnavailable      = "unavailable"
)

// Subscribed are the events the engine subscribes to (contract §1.3).
var Subscribed = []string{
	"projects_v2_item", "issues", "sub_issues", "issue_dependencies", "pull_request", "pull_request_review",
	"check_run", "check_suite", "status", "push", "installation_repositories",
}

// EventLabel is the event label of engine_webhook_deliveries_total: a subscribed event or ping as is,
// anything else "other".
func EventLabel(event string) string {
	if event == "ping" || slices.Contains(Subscribed, event) {
		return event
	}
	return "other"
}

// Config configures the receiver.
type Config struct {
	// Path is the one path served (default /github/webhook); every other path is 404.
	Path string
	// MaxBodyBytes caps the body, inclusive (default 10 MiB).
	MaxBodyBytes int64
	// MaxInFlightBytes bounds the bodies the receiver holds at once (being read, verified and stored),
	// by their declared size (Content-Length, or MaxBodyBytes without one). A request that doesn't fit
	// is answered 503 at once, before its body is read, and counted unavailable: unsigned bodies can't
	// exhaust the memory, and GitHub logs the delivery as failed, which the engine finds in the
	// delivery log. Default DefaultInFlightBodies × MaxBodyBytes; never less than MaxBodyBytes.
	MaxInFlightBytes int64
	// Secret is the App's webhook secret (required).
	Secret []byte
	// Store stores verified deliveries (required).
	Store inbox.Store
	// Deliveries is engine_webhook_deliveries_total{event,outcome}, created and registered by the caller;
	// nil counts nothing.
	Deliveries *prometheus.CounterVec
	// Clock stamps received_at (required).
	Clock clock.Clock
	// ProjectNodeIDs, when set, are the boards the engine observes: projects_v2_item deliveries about
	// any other project of the org are acknowledged and ignored, not stored.
	ProjectNodeIDs []string
	// StoreTimeout bounds storing one delivery (default 10s).
	StoreTimeout time.Duration
	// Tracker, when set, tracks each request in flight (settle).
	Tracker *settle.Tracker
	Logger  logr.Logger
}

// Validate reports a configuration New would refuse.
func (c Config) Validate() error {
	switch {
	case len(c.Secret) == 0:
		return errors.New("receiver: no webhook secret")
	case c.Store == nil:
		return errors.New("receiver: no store")
	case c.Clock == nil:
		return errors.New("receiver: no clock")
	}
	return nil
}

type handler struct {
	cfg      Config
	inFlight budget
}

// budget is a non-blocking semaphore weighted by bytes.
type budget struct {
	mu   sync.Mutex
	used int64
	max  int64
}

// take reserves n bytes, if they fit.
func (b *budget) take(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.max {
		return false
	}
	b.used += n
	return true
}

func (b *budget) give(n int64) {
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
}

// New returns the receiver's http.Handler. It panics on a configuration Validate refuses (an empty
// secret would accept any signature).
func New(cfg Config) http.Handler {
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	if cfg.Path == "" {
		cfg.Path = DefaultPath
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if cfg.StoreTimeout <= 0 {
		cfg.StoreTimeout = DefaultStoreTimeout
	}
	if cfg.MaxInFlightBytes <= 0 {
		cfg.MaxInFlightBytes = DefaultInFlightBodies * cfg.MaxBodyBytes
	}
	cfg.MaxInFlightBytes = max(cfg.MaxInFlightBytes, cfg.MaxBodyBytes)
	cfg.Secret = slices.Clone(cfg.Secret)
	return &handler{cfg: cfg, inFlight: budget{max: cfg.MaxInFlightBytes}}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != h.cfg.Path {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer h.cfg.Tracker.Begin("webhook request")()

	event := r.Header.Get("X-GitHub-Event")
	label := EventLabel(event)
	if r.ContentLength > h.cfg.MaxBodyBytes {
		h.answer(w, label, OutcomeTooLarge, http.StatusRequestEntityTooLarge)
		return
	}
	size := r.ContentLength
	if size < 0 {
		size = h.cfg.MaxBodyBytes
	}
	if !h.inFlight.take(size) {
		h.cfg.Logger.V(1).Info("over the in-flight budget: delivery refused unread", "event", event,
			"delivery", r.Header.Get("X-GitHub-Delivery"), "bytes", r.ContentLength)
		h.answer(w, label, OutcomeUnavailable, http.StatusServiceUnavailable)
		return
	}
	defer h.inFlight.give(size)
	body, err := readBody(http.MaxBytesReader(w, r.Body, h.cfg.MaxBodyBytes), r.ContentLength)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.answer(w, label, OutcomeTooLarge, http.StatusRequestEntityTooLarge)
			return
		}
		// The client went away mid-body: not a delivery.
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	sig := r.Header.Get("X-Hub-Signature-256")
	if sig == "" {
		h.answer(w, label, OutcomeMissingSignature, http.StatusUnauthorized)
		return
	}
	if !h.valid(sig, body) {
		h.answer(w, label, OutcomeBadSignature, http.StatusUnauthorized)
		return
	}
	// Verified from here on.
	if label == "ping" || label == "other" {
		h.answer(w, label, OutcomeIgnored, http.StatusOK)
		return
	}
	id := r.Header.Get("X-GitHub-Delivery")
	var p payload
	if id == "" || json.Unmarshal(body, &p) != nil {
		// GitHub always sends both; without them there is nothing to deduplicate or wake.
		h.answer(w, label, OutcomeIgnored, http.StatusBadRequest)
		return
	}
	if event == "projects_v2_item" && len(h.cfg.ProjectNodeIDs) > 0 &&
		(p.ProjectsV2Item == nil || !slices.Contains(h.cfg.ProjectNodeIDs, p.ProjectsV2Item.ProjectNodeID)) {
		h.answer(w, label, OutcomeIgnored, http.StatusOK)
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), h.cfg.StoreTimeout)
	defer cancel()
	dup, err := h.cfg.Store.Put(ctx, inbox.Delivery{
		ID:         id,
		Event:      event,
		Action:     p.Action,
		ReceivedAt: h.cfg.Clock.Now(),
		Payload:    body,
	})
	switch {
	case err != nil:
		h.cfg.Logger.Error(err, "delivery not stored", "delivery", id, "event", event)
		h.answer(w, label, OutcomeUnavailable, http.StatusServiceUnavailable)
	case dup:
		h.answer(w, label, OutcomeDuplicate, http.StatusOK)
	default:
		h.answer(w, label, OutcomeAccepted, http.StatusAccepted)
	}
}

// readBody reads a body of the declared length into one allocation, or, without one, as it comes.
func readBody(r io.Reader, length int64) ([]byte, error) {
	if length < 0 {
		return io.ReadAll(r)
	}
	b := make([]byte, length)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

// payload is all the receiver reads from a body.
type payload struct {
	Action         string `json:"action"`
	ProjectsV2Item *struct {
		ProjectNodeID string `json:"project_node_id"`
	} `json:"projects_v2_item"`
}

// valid checks "sha256=<hex HMAC-SHA256 of body>" in constant time.
func (h *handler) valid(sig string, body []byte) bool {
	hexSum, ok := strings.CutPrefix(sig, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(hexSum)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, h.cfg.Secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func (h *handler) answer(w http.ResponseWriter, event, outcome string, code int) {
	if h.cfg.Deliveries != nil {
		h.cfg.Deliveries.With(prometheus.Labels{"event": event, "outcome": outcome}).Inc()
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, outcome+"\n")
}

// Sign returns the X-Hub-Signature-256 value GitHub sends for body (for fakes and tests).
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
