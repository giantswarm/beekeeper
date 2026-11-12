//go:build scenario

package fakegithub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// clock is a scenario clock a test moves.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fixture is the GitHub most tests start with: three issues (the first
// on the board as Ready and the parent of the second, the third closed)
// and a pull request that closes the first, with a pending check and an
// approval.
const fixture = `
repositories:
  - name: example/app
    labels: [{name: bug, color: d73a4a}]
    issues:
      - {number: 1, title: First, board: Ready, labels: [bug]}
      - {number: 2, title: Second, board: Todo, parent: 1}
      - {number: 3, title: Third, state: closed}
    pulls:
      - number: 4
        title: Fix the first
        head: fix-1
        body: "Closes #1"
        checks: [{name: build, state: pending}]
        reviews: [{author: reviewer, state: approved}]
`

// parseGiven decodes given.github as a scenario's loader does: strictly.
func parseGiven(t *testing.T, doc string) *Given {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(doc))
	dec.KnownFields(true)
	var g Given
	if err := dec.Decode(&g); err != nil {
		t.Fatalf("given.github: %v", err)
	}
	return &g
}

// build is the scenario's GitHub of a given.github document on a clock
// the test moves.
func build(t *testing.T, doc string) (*Server, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)}
	s, err := New(parseGiven(t, doc), Options{Clock: c.Now, BoardOwner: "example", BoardNumber: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, c
}

// gh runs the fake gh as caller.
func gh(s *Server, caller string, args ...string) GHResult {
	return s.GH(context.Background(), GHCall{Args: args, Env: map[string]string{CallerEnv: caller}})
}

// mustGH runs the fake gh and fails the test unless it exits 0.
func mustGH(t *testing.T, s *Server, args ...string) string {
	t.Helper()
	res := gh(s, "test", args...)
	if !res.Handled || res.Exit != 0 {
		t.Fatalf("gh %s: handled %t (%s), exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), res.Handled, res.Reason, res.Exit, res.Stdout, res.Stderr)
	}
	return res.Stdout
}

// request sends one request over the server's own entry, as caller.
func request(s *Server, caller, method, target string, header map[string]string, body string) *Response {
	path, rawQuery, _ := strings.Cut(target, "?")
	query, _ := url.ParseQuery(rawQuery)
	h := http.Header{}
	for k, v := range header {
		h.Set(k, v)
	}
	return s.Do(context.Background(), &Request{Method: method, Path: path, Query: query, Header: h, Body: []byte(body), Caller: caller, Via: viaHTTP})
}

// graphql sends a GraphQL query as caller and decodes its answer.
func graphql(t *testing.T, s *Server, caller, query string, vars map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		t.Fatal(err)
	}
	resp := request(s, caller, http.MethodPost, "/graphql", nil, string(body))
	if resp.Status != http.StatusOK {
		t.Fatalf("graphql: status %d: %s", resp.Status, resp.Body)
	}
	return decode(t, resp.Body)
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("not a JSON object: %v: %s", err, b)
	}
	return out
}

func decodeList(t *testing.T, b []byte) []any {
	t.Helper()
	var out []any
	if err := json.Unmarshal(bytes.TrimSpace(b), &out); err != nil {
		t.Fatalf("not a JSON array: %v: %s", err, b)
	}
	return out
}

// at walks a decoded JSON value by keys and list indexes.
func at(t *testing.T, v any, path ...any) any {
	t.Helper()
	cur := v
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("at %v: %v is no object at %q", path, cur, k)
			}
			cur = m[k]
		case int:
			l, ok := cur.([]any)
			if !ok || k >= len(l) {
				t.Fatalf("at %v: %v has no index %d", path, cur, k)
			}
			cur = l[k]
		}
	}
	return cur
}

// spent is what caller spent of a bucket.
func spent(s *Server, caller, bucket string) int {
	for _, sp := range s.Spent() {
		if sp.Caller == caller && sp.Bucket == bucket {
			return sp.Points
		}
	}
	return 0
}

func ptr[T any](v T) *T { return &v }
