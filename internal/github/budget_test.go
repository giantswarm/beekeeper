package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// graphqlServer answers the GraphQL endpoint with status, headers and body.
func graphqlServer(t *testing.T, status int, headers map[string]string, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" || r.Header.Get("Authorization") != "Bearer tok" {
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
			want: GraphQL{Limit: 5000, Reset: reset, Refused: "API rate limit exceeded for user ID 1."}, wantBlock: true,
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
