package jev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The answers of the ready? requests the tests send.
const (
	answerReady    = "ready"
	answerNotReady = "not-ready"
)

func readyRequest() Request {
	return Request{
		Point:   "ready?",
		Subject: "giantswarm/roadmap#101",
		Answers: []string{answerReady, answerNotReady},
		Fields:  map[string]any{"title": "Retry transient errors", "body": "### Acceptance criteria\n- [ ] retries"},
		Rule:    "RULE-SENTINEL",
	}
}

func TestHTTPAsk(t *testing.T) {
	t.Parallel()
	var got struct {
		method, path, auth, contentType string
		body                            []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path
		got.auth, got.contentType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		got.body, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"model":"jev-1.14.0","answers":{"answer":{"type":"choice","choice":"not-ready",
			"confidence":0.8,"probabilities":{"ready":0.2,"not-ready":0.8}}},"usage":{"input_tokens":80,"output_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)

	c := NewHTTP(HTTPConfig{URL: srv.URL + "/v1/systemone", Key: "test-key", HTTPClient: srv.Client(), Timeout: 5 * time.Second})
	a, err := c.Ask(context.Background(), readyRequest())
	if err != nil {
		t.Fatal(err)
	}
	if a != (Answer{Answer: answerNotReady, Confidence: 0.8}) {
		t.Errorf("answer = %+v", a)
	}
	if got.method != http.MethodPost || got.path != "/v1/systemone" {
		t.Errorf("request = %s %s, want POST /v1/systemone", got.method, got.path)
	}
	if got.auth != "Bearer test-key" || got.contentType != "application/json" {
		t.Errorf("headers: Authorization %q, Content-Type %q", got.auth, got.contentType)
	}
	if strings.Contains(string(got.body), "RULE-SENTINEL") || strings.Contains(string(got.body), `"rule"`) {
		t.Errorf("the rule's answer was sent: %s", got.body)
	}

	var sent struct {
		Model string `json:"model"`
		State struct {
			Point   string         `json:"point"`
			Subject string         `json:"subject"`
			Fields  map[string]any `json:"fields"`
		} `json:"state"`
		Questions map[string]struct {
			Type         string         `json:"type"`
			Instructions string         `json:"instructions"`
			Criteria     map[string]any `json:"criteria"`
		} `json:"questions"`
	}
	dec := json.NewDecoder(strings.NewReader(string(got.body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sent); err != nil {
		t.Fatalf("request body %s: %v", got.body, err)
	}
	if sent.Model != DefaultModel || sent.State.Point != "ready?" || sent.State.Subject != "giantswarm/roadmap#101" {
		t.Errorf("model %q, point %q, subject %q", sent.Model, sent.State.Point, sent.State.Subject)
	}
	var fields []string
	for k := range sent.State.Fields {
		fields = append(fields, k)
	}
	sort.Strings(fields)
	if !slices.Equal(fields, []string{"body", "title"}) {
		t.Errorf("fields sent = %v, want body and title", fields)
	}
	q, ok := sent.Questions[questionID]
	if !ok || len(sent.Questions) != 1 || q.Type != "choice" || q.Instructions == "" {
		t.Fatalf("questions = %+v", sent.Questions)
	}
	var options []string
	for k := range q.Criteria {
		options = append(options, k)
	}
	sort.Strings(options)
	if !slices.Equal(options, []string{answerNotReady, answerReady}) {
		t.Errorf("options = %v", options)
	}
}

func TestHTTPAskErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status int
		body   string
		req    func(*Request)
		key    string
		kind   string
		called bool
	}{
		{name: "server error", status: 500, body: `{"detail":"boom"}`, kind: KindServer, called: true},
		{name: "overloaded", status: 529, body: `{"detail":"overloaded"}`, kind: KindServer, called: true},
		{name: "rate limited", status: 429, body: `{"detail":"slow down"}`, kind: KindRateLimited, called: true},
		{name: "bad request", status: 400, body: `{"detail":"max_tokens_exceeded"}`, kind: KindRejected, called: true},
		{name: "unauthorized", status: 401, body: ``, kind: KindRejected, called: true},
		{name: "unprocessable", status: 422, body: `{"detail":[{"loc":["body"],"msg":"x","type":"y"}]}`, kind: KindRejected, called: true},
		{name: "not json", status: 200, body: `<html>`, kind: KindInvalid, called: true},
		{name: "no answer", status: 200, body: `{"answers":{}}`, kind: KindInvalid, called: true},
		{name: "answer not allowed", status: 200, body: `{"answers":{"answer":{"type":"choice","choice":"maybe","confidence":0.9}}}`, kind: KindInvalid, called: true},
		{name: "no confidence", status: 200, body: `{"answers":{"answer":{"type":"choice","choice":"ready"}}}`, kind: KindInvalid, called: true},
		{name: "no allowed answers", req: func(r *Request) { r.Answers = nil }, kind: KindInvalid},
		{name: "no key", key: "-", kind: KindRejected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(srv.Close)
			key := "test-key"
			if tt.key == "-" {
				key = ""
			}
			req := readyRequest()
			if tt.req != nil {
				tt.req(&req)
			}
			_, err := NewHTTP(HTTPConfig{URL: srv.URL, Key: key, HTTPClient: srv.Client()}).Ask(context.Background(), req)
			if err == nil {
				t.Fatal("no error")
			}
			if got := ErrorKind(err); got != tt.kind {
				t.Errorf("ErrorKind(%v) = %q, want %q", err, got, tt.kind)
			}
			if tt.status != 0 {
				var e *Error
				if !errors.As(err, &e) || e.Status != tt.status {
					t.Errorf("error %v carries no status %d", err, tt.status)
				}
			}
			if called := calls.Load() > 0; called != tt.called {
				t.Errorf("called = %v, want %v", called, tt.called)
			}
		})
	}
}

func TestHTTPAskTimeout(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	start := time.Now()
	_, err := NewHTTP(HTTPConfig{URL: srv.URL, Key: "k", HTTPClient: srv.Client(), Timeout: 20 * time.Millisecond}).
		Ask(context.Background(), readyRequest())
	if got := ErrorKind(err); got != KindTimeout {
		t.Errorf("ErrorKind(%v) = %q, want timeout", err, got)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("%v doesn't wrap the deadline", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("timed out after %s", d)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = NewHTTP(HTTPConfig{URL: srv.URL, Key: "k", HTTPClient: srv.Client()}).Ask(ctx, readyRequest())
	if got := ErrorKind(err); got != KindCanceled {
		t.Errorf("ErrorKind(%v) = %q, want canceled", err, got)
	}
}

func TestErrorKind(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{&Error{Kind: KindServer, Status: 503, Err: errors.New("x")}, KindServer},
		{fmt.Errorf("wrapped: %w", &Error{Kind: KindTimeout, Err: context.DeadlineExceeded}), KindTimeout},
		{context.DeadlineExceeded, KindTimeout},
		{context.Canceled, KindCanceled},
		{errors.New("connection refused"), KindServer},
	} {
		if got := ErrorKind(tt.err); got != tt.want {
			t.Errorf("ErrorKind(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

func TestNewHTTPDefaults(t *testing.T) {
	t.Parallel()
	h := NewHTTP(HTTPConfig{Key: "k"})
	if h.url != DefaultURL || h.model != DefaultModel || h.timeout != DefaultTimeout || h.client != http.DefaultClient {
		t.Errorf("defaults: %+v", h)
	}
}
