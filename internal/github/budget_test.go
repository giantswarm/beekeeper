package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// bearerTok is the Authorization header of the fake token "tok".
const bearerTok = "Bearer tok"

// graphqlServer answers the GraphQL endpoint with status, headers and body.
func graphqlServer(t *testing.T, status int, headers map[string]string, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" || r.Header.Get("Authorization") != bearerTok {
			t.Errorf("request %s %s, want an authorized POST /graphql", r.Method, r.URL.Path)
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("BEEKEEPER_GITHUB_API", srv.URL)
}

func TestProbeGraphQL(t *testing.T) {
	now := time.Date(2026, 10, 6, 3, 50, 0, 0, time.UTC)
	reset := time.Date(2026, 10, 6, 3, 55, 12, 0, time.UTC)
	limits := map[string]string{"X-RateLimit-Limit": "5000", "X-RateLimit-Remaining": "4960", "X-RateLimit-Used": "40",
		"X-RateLimit-Reset": "1791258912", "X-RateLimit-Resource": "graphql"}
	for name, tc := range map[string]struct {
		status    int
		headers   map[string]string
		body      string
		want      GraphQL
		wantErr   string
		wantBlock bool
	}{
		"answers": {
			status: 200, headers: limits,
			body: `{"data":{"rateLimit":{"limit":5000,"remaining":4960,"used":40,"resetAt":"2026-10-06T03:55:12Z"}}}`,
			want: GraphQL{Limit: 5000, Remaining: 4960, Used: 40, Reset: reset},
		},
		"the hourly limit spent": {
			status: 200, headers: limits,
			body: `{"data":{"rateLimit":{"limit":5000,"remaining":0,"used":5000,"resetAt":"2026-10-06T03:55:12Z"}}}`,
			want: GraphQL{Limit: 5000, Remaining: 0, Used: 5000, Reset: reset, Refused: "the GraphQL limit is spent"}, wantBlock: true,
		},
		"refused with headroom: a secondary limit without a time": {
			status: 200, headers: limits,
			body:      `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit already exceeded for user ID 1."}]}`,
			want:      GraphQL{Limit: 5000, Remaining: 4960, Used: 40, Refused: "API rate limit already exceeded for user ID 1.", Secondary: true},
			wantBlock: true,
		},
		"a secondary limit with Retry-After": {
			status: 403, headers: map[string]string{"Retry-After": "60"},
			body: `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`,
			want: GraphQL{Reset: now.Add(time.Minute), Secondary: true,
				Refused: "You have exceeded a secondary rate limit. Please wait a few minutes before you try again."},
			wantBlock: true,
		},
		"the limit spent, refused with 403": {
			status: 403, headers: map[string]string{"X-RateLimit-Limit": "5000", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1791258912"},
			body: `{"message":"API rate limit exceeded for user ID 1."}`,
			want: GraphQL{Limit: 5000, Used: 5000, Reset: reset, Refused: "API rate limit exceeded for user ID 1."}, wantBlock: true,
		},
		"a server error is no refusal": {status: 502, body: `bad gateway`, wantErr: "502"},
	} {
		t.Run(name, func(t *testing.T) {
			graphqlServer(t, tc.status, tc.headers, tc.body)
			got := ProbeGraphQL(context.Background(), http.DefaultClient, "tok", now)
			if tc.wantErr != "" {
				if !strings.Contains(got.Err, tc.wantErr) {
					t.Fatalf("Err = %q, want it to name %q", got.Err, tc.wantErr)
				}
				return
			}
			if !got.Reset.Equal(tc.want.Reset) {
				t.Errorf("Reset = %v, want %v", got.Reset, tc.want.Reset)
			}
			got.Reset, tc.want.Reset = time.Time{}, time.Time{}
			if got != tc.want {
				t.Errorf("ProbeGraphQL = %+v, want %+v", got, tc.want)
			}
			if b := got.Blocks(now); b != tc.wantBlock {
				t.Errorf("Blocks = %v, want %v", b, tc.wantBlock)
			}
		})
	}
}

func TestBlocksEndsAtTheReset(t *testing.T) {
	now := time.Now()
	g := &GraphQL{Refused: "spent", Reset: now.Add(-time.Second)}
	if g.Blocks(now) {
		t.Error("a refusal past its reset still blocks")
	}
	if (*GraphQL)(nil).Blocks(now) {
		t.Error("no reading blocks")
	}
}

func TestProbeSearch(t *testing.T) {
	reset := time.Unix(1791258912, 0)
	for name, tc := range map[string]struct {
		status  int
		body    string
		want    Searches
		wantErr string
	}{
		"both limits": {
			status: 200,
			body: `{"resources":{"core":{"limit":5000,"remaining":5000,"used":0,"reset":1791258912},` +
				`"search":{"limit":30,"remaining":28,"used":2,"reset":1791258912},` +
				`"code_search":{"limit":10,"remaining":0,"used":10,"reset":1791258912}}}`,
			want: Searches{Search: &Limit{Limit: 30, Remaining: 28, Used: 2, Reset: reset},
				CodeSearch: &Limit{Limit: 10, Remaining: 0, Used: 10, Reset: reset}},
		},
		"no search limits": {status: 200, body: `{"resources":{"core":{"limit":5000}}}`, wantErr: "no search limits"},
		"a server error":   {status: 502, body: `bad gateway`, wantErr: "502"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/rate_limit" || r.Header.Get("Authorization") != bearerTok {
					t.Errorf("request %s %s, want an authorized GET /rate_limit", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			t.Setenv("BEEKEEPER_GITHUB_API", srv.URL)
			got := ProbeSearch(t.Context(), http.DefaultClient, "tok")
			if tc.wantErr != "" {
				if !strings.Contains(got.Err, tc.wantErr) {
					t.Fatalf("Err = %q, want it to name %q", got.Err, tc.wantErr)
				}
				return
			}
			if got.Err != "" || *got.Search != *tc.want.Search || *got.CodeSearch != *tc.want.CodeSearch {
				t.Errorf("ProbeSearch = %+v %+v %q, want %+v %+v", got.Search, got.CodeSearch, got.Err, tc.want.Search, tc.want.CodeSearch)
			}
		})
	}
}

func TestLimitSpent(t *testing.T) {
	now := time.Now()
	for name, tc := range map[string]struct {
		l    *Limit
		want bool
	}{
		"nothing left before the reset": {&Limit{Limit: 10, Reset: now.Add(time.Second)}, true},
		"nothing left past the reset":   {&Limit{Limit: 10, Reset: now.Add(-time.Second)}, false},
		"one left":                      {&Limit{Limit: 10, Remaining: 1, Reset: now.Add(time.Second)}, false},
		"no reading":                    {nil, false},
	} {
		if got := tc.l.Spent(now); got != tc.want {
			t.Errorf("%s: Spent = %v, want %v", name, got, tc.want)
		}
	}
}

// gh's words for a refusal under a rate limit, REST's and GraphQL's, are
// a rate limit; GitHub not answering is none.
func TestRateLimited(t *testing.T) {
	for msg, want := range map[string]bool{
		"GraphQL: API rate limit already exceeded for user ID 1.":         true,
		"gh api repos/o/r/pulls/7: HTTP 403: API rate limit exceeded for": true,
		"HTTP 403: You have exceeded a secondary rate limit.":             true,
		"connection reset by peer":                                        false,
		"HTTP 404: Not Found":                                             false,
	} {
		if got := RateLimited(errors.New(msg)); got != want {
			t.Errorf("RateLimited(%q) = %v, want %v", msg, got, want)
		}
	}
	if RateLimited(nil) {
		t.Error("no error is a rate limit")
	}
}

// SpentUntil is the later reset of the limits spent at now: the core
// budget with nothing left, the GraphQL limit refused or with nothing left.
func TestSpentUntil(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	core, gql := now.Add(20*time.Minute), now.Add(40*time.Minute)
	for name, c := range map[string]struct {
		b    Budget
		want time.Time
	}{
		"nothing spent":        {Budget{Limit: 5000, Remaining: 4000, Reset: core, GraphQL: &GraphQL{Limit: 5000, Remaining: 10, Reset: gql}}, time.Time{}},
		"GraphQL refused":      {Budget{Limit: 5000, Remaining: 4000, Reset: core, GraphQL: &GraphQL{Refused: "API rate limit exceeded", Reset: gql}}, gql},
		"GraphQL spent":        {Budget{Limit: 5000, Remaining: 4000, Reset: core, GraphQL: &GraphQL{Limit: 5000, Reset: gql}}, gql},
		"core spent":           {Budget{Limit: 5000, Reset: core}, core},
		"both, the later":      {Budget{Limit: 5000, Reset: core, GraphQL: &GraphQL{Refused: "x", Reset: gql}}, gql},
		"a refusal, no reset":  {Budget{Limit: 5000, Remaining: 4000, Reset: core, GraphQL: &GraphQL{Refused: "x"}}, time.Time{}},
		"a reset already past": {Budget{Limit: 5000, Reset: now.Add(-time.Minute)}, time.Time{}},
	} {
		if got := c.b.SpentUntil(now); !got.Equal(c.want) {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}
