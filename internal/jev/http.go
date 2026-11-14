package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Defaults of the HTTP client.
const (
	DefaultURL     = "https://api.typesafe.ai/v1/systemone"
	DefaultModel   = "jev-latest"
	DefaultTimeout = 30 * time.Second

	// questionID names the one question of a request.
	questionID = "answer"
	// maxResponse bounds the response body read.
	maxResponse = 1 << 20
	// maxChoices is the API's limit on a choice question's options.
	maxChoices = 255
)

// Error kinds, recorded as a decision's jevError.
const (
	KindTimeout     = "timeout"      // the call's deadline passed
	KindCanceled    = "canceled"     // the caller canceled the call
	KindServer      = "server"       // 5xx (529 included) or no response
	KindRateLimited = "rate-limited" // 429
	KindRejected    = "rejected"     // another 4xx, or no API key
	KindInvalid     = "invalid"      // a request JEV can't take, or an answer that can't be used
)

// Error is a failed JEV call.
type Error struct {
	// Kind is timeout, canceled, server, rate-limited, rejected or invalid.
	Kind string
	// Status is the HTTP status, 0 when there was no response.
	Status int
	Err    error
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("jev %s (HTTP %d): %v", e.Kind, e.Status, e.Err)
	}
	return fmt.Sprintf("jev %s: %v", e.Kind, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// ErrorKind returns the kind of a JEV error for the decision's jevError: an *Error's kind, timeout for
// a passed deadline, canceled, else server. It is empty for nil.
func ErrorKind(err error) string {
	var e *Error
	var ne net.Error
	switch {
	case err == nil:
		return ""
	case errors.As(err, &e):
		return e.Kind
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return KindTimeout
	case errors.Is(err, context.Canceled):
		return KindCanceled
	default:
		return KindServer
	}
}

// HTTPConfig configures the systemone client.
type HTTPConfig struct {
	// URL is systemone's endpoint; DefaultURL when empty.
	URL string
	// Key is the TypeSafe API key, sent as a Bearer token.
	Key string
	// HTTPClient makes the calls; http.DefaultClient when nil. The call's deadline is Timeout, through
	// the request's context.
	HTTPClient *http.Client
	// Timeout bounds one call; DefaultTimeout when zero.
	Timeout time.Duration
	// Model is the model or alias to ask; DefaultModel when empty.
	Model string
}

// HTTP asks JEV through TypeSafe's systemone API: one choice question per request, whose options are
// the allowed answers. It sends the point, the subject, the answers and the fields, never the rule's
// answer. It doesn't retry: a failed call is recorded as jev_error and the rule stands.
type HTTP struct {
	url, key, model string
	client          *http.Client
	timeout         time.Duration
}

var _ Client = (*HTTP)(nil)

// NewHTTP returns a client for systemone.
func NewHTTP(c HTTPConfig) *HTTP {
	h := &HTTP{url: c.URL, key: c.Key, model: c.Model, client: c.HTTPClient, timeout: c.Timeout}
	if h.url == "" {
		h.url = DefaultURL
	}
	if h.model == "" {
		h.model = DefaultModel
	}
	if h.client == nil {
		h.client = http.DefaultClient
	}
	if h.timeout <= 0 {
		h.timeout = DefaultTimeout
	}
	return h
}

// The wire format (docs.typesafe.ai): a state, and questions by id.
type (
	wireRequest struct {
		Model     string                  `json:"model"`
		State     wireState               `json:"state"`
		Questions map[string]wireQuestion `json:"questions"`
	}
	wireState struct {
		Point   string         `json:"point"`
		Subject string         `json:"subject,omitempty"`
		Fields  map[string]any `json:"fields,omitempty"`
	}
	wireQuestion struct {
		Type         string             `json:"type"`
		Instructions string             `json:"instructions"`
		Criteria     map[string]*string `json:"criteria"`
	}
	wireResponse struct {
		Model   string                `json:"model"`
		Answers map[string]wireAnswer `json:"answers"`
	}
	wireAnswer struct {
		Type       string   `json:"type"`
		Choice     string   `json:"choice"`
		Confidence *float64 `json:"confidence"`
	}
)

// instructions are the questions of the phase 1 decision points (contract §1.4).
var instructions = map[string]string{
	"ready?": "Is this issue ready to be implemented? It is ready when it states acceptance criteria " +
		"with at least one item and the repository the work lands in.",
	"next?":          "Which of the candidate items should this person start next?",
	"parallel-safe?": "Can these two items be implemented at the same time in the same repository without their changes conflicting?",
	"bug-severity?":  "How severe is this bug?",
}

// Ask asks JEV one question. Errors and timeouts are returned as *Error.
func (h *HTTP) Ask(ctx context.Context, r Request) (Answer, error) {
	if h.key == "" {
		return Answer{}, &Error{Kind: KindRejected, Err: errors.New("no API key configured")}
	}
	if len(r.Answers) == 0 || len(r.Answers) > maxChoices {
		return Answer{}, &Error{Kind: KindInvalid, Err: fmt.Errorf("%s needs 1 to %d allowed answers, got %d", r.Point, maxChoices, len(r.Answers))}
	}
	q := wireQuestion{Type: "choice", Instructions: instructions[r.Point], Criteria: map[string]*string{}}
	if q.Instructions == "" {
		q.Instructions = fmt.Sprintf("Answer the decision point %q for the subject.", r.Point)
	}
	for _, a := range r.Answers {
		q.Criteria[a] = nil
	}
	body, err := json.Marshal(wireRequest{
		Model:     h.model,
		State:     wireState{Point: r.Point, Subject: r.Subject, Fields: r.Fields},
		Questions: map[string]wireQuestion{questionID: q},
	})
	if err != nil {
		return Answer{}, &Error{Kind: KindInvalid, Err: err}
	}

	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return Answer{}, &Error{Kind: KindInvalid, Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+h.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return Answer{}, &Error{Kind: transportKind(ctx, err), Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return Answer{}, &Error{Kind: transportKind(ctx, err), Status: resp.StatusCode, Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Answer{}, &Error{Kind: statusKind(resp.StatusCode), Status: resp.StatusCode, Err: errors.New(snippet(data))}
	}

	var wr wireResponse
	if err := json.Unmarshal(data, &wr); err != nil {
		return Answer{}, &Error{Kind: KindInvalid, Status: resp.StatusCode, Err: fmt.Errorf("decoding the answer: %w", err)}
	}
	a, ok := wr.Answers[questionID]
	switch {
	case !ok:
		return Answer{}, &Error{Kind: KindInvalid, Status: resp.StatusCode, Err: errors.New("no answer to the question")}
	case !slices.Contains(r.Answers, a.Choice):
		return Answer{}, &Error{Kind: KindInvalid, Status: resp.StatusCode, Err: fmt.Errorf("answer %q is not one of %v", a.Choice, r.Answers)}
	case a.Confidence == nil:
		return Answer{}, &Error{Kind: KindInvalid, Status: resp.StatusCode, Err: errors.New("answer without a confidence")}
	}
	return Answer{Answer: a.Choice, Confidence: *a.Confidence}, nil
}

func transportKind(ctx context.Context, err error) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return KindTimeout
	case errors.Is(ctx.Err(), context.Canceled):
		return KindCanceled
	}
	return ErrorKind(err)
}

func statusKind(code int) string {
	switch {
	case code == http.StatusTooManyRequests:
		return KindRateLimited
	case code >= 500:
		return KindServer
	default:
		return KindRejected
	}
}

// snippet is the start of an error body, on one line.
func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		s = "empty response"
	}
	return s
}
