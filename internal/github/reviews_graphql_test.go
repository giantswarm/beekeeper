package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const graphReviewHead = `{"data":{"viewer":{"login":"fixture-person"},"repository":{"pullRequest":{"id":"P7","headRefOid":"h","author":{"login":"fixture-person","type":"User"},"commits":{"nodes":[{"commit":{"oid":"h","committedDate":"2026-10-21T08:00:00Z"}}]}}}}`

func reviewPage(nodes, cursor string) []byte {
	return []byte(fmt.Sprintf(`{"data":{"node":{"items":{"nodes":%s,"pageInfo":{"hasNextPage":%t,"endCursor":%q}}}}}`, nodes, cursor != "", cursor))
}

// GraphQL supplies every page while REST has no quota. The conversion keeps
// the same persistent changes-requested and unanswered-comment semantics.
func TestReviewGuardWithoutRESTQuota(t *testing.T) {
	for _, verdict := range []string{"CHANGES_REQUESTED", "APPROVED"} {
		t.Run(verdict, func(t *testing.T) {
			calls := map[string]int{}
			gh := func(ctx context.Context, args ...string) ([]byte, error) {
				if Repo(ctx) != "o/r" {
					t.Fatalf("repository context lost: %q", Repo(ctx))
				}
				if len(args) < 4 || args[1] != "graphql" {
					t.Fatal("REST called despite healthy GraphQL")
				}
				q := args[len(args)-1]
				calls[q]++
				switch {
				case strings.Contains(q, "viewer"):
					return []byte(graphReviewHead), nil
				case strings.Contains(q, "items:reviews"):
					if !strings.Contains(q, "after:") {
						return reviewPage(`[]`, "reviews-next"), nil
					}
					return reviewPage(fmt.Sprintf(`[{"id":"R1","user":{"login":"pat","type":"User"},"state":%q,"submitted_at":"2026-10-21T07:00:00Z","commit":{"oid":"old"},"html_url":"review","comments":{"totalCount":1}}]`, verdict), ""), nil
				case strings.Contains(q, `node(id:"R1")`):
					if !strings.Contains(q, "after:") {
						return reviewPage(`[]`, "diff-next"), nil
					}
					return reviewPage(`[{"user":{"login":"kim","type":"User"},"created_at":"2026-10-21T07:30:00Z","originalCommit":{"oid":"h"},"html_url":"diff"}]`, ""), nil
				case strings.Contains(q, "items:comments"):
					if !strings.Contains(q, "after:") {
						return reviewPage(`[]`, "comments-next"), nil
					}
					return reviewPage(`[{"user":{"login":"alex","type":"User"},"created_at":"2026-10-21T09:00:00Z","html_url":"conversation"},{"user":{"login":"robot","type":"Bot"},"created_at":"2026-10-21T09:30:00Z","html_url":"bot"}]`, ""), nil
				}
				return nil, fmt.Errorf("unexpected query %s", q)
			}
			why, open, err := ReviewGuard(context.Background(), gh, "o/r", 7)
			want := 2
			if verdict == changesRequested {
				want++
			}
			if err != nil || len(open) != want || !strings.Contains(why, "kim") || !strings.Contains(why, "alex") || strings.Contains(why, "robot") {
				t.Fatalf("why=%q open=%+v err=%v", why, open, err)
			}
			if len(calls) != 7 {
				t.Fatalf("pages read=%d, want 7", len(calls))
			}
		})
	}
}

func TestReviewGuardGraphQLFailsClosed(t *testing.T) {
	for name, page := range map[string]string{
		"partial error":      `{"data":{"node":{"items":{"nodes":[],"pageInfo":{"hasNextPage":false}}}},"errors":[{"message":"API rate limit exceeded"}]}`,
		"null node":          `{"data":{"node":null}}`,
		"null connection":    `{"data":{"node":{"items":null}}}`,
		"null item":          `{"data":{"node":{"items":{"nodes":[null],"pageInfo":{"hasNextPage":false}}}}}`,
		"missing pagination": `{"data":{"node":{"items":{"nodes":[]}}}}`,
		"repeated cursor":    string(reviewPage(`[]`, "same")),
		"broken JSON":        `{`,
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			gh := func(_ context.Context, args ...string) ([]byte, error) {
				calls++
				if calls > 6 {
					t.Fatal("unbounded pagination")
				}
				if args[1] != "graphql" {
					return nil, errors.New("API rate limit exceeded")
				}
				if strings.Contains(args[len(args)-1], "viewer") {
					return []byte(graphReviewHead), nil
				}
				return []byte(page), nil
			}
			why, open, err := ReviewGuard(context.Background(), gh, "o/r", 7)
			if err == nil || why != "" || open != nil {
				t.Fatalf("incomplete read allowed merge: %q %+v %v", why, open, err)
			}
		})
	}
}

func TestViewerLoginUsesAvailableAPIAndRunner(t *testing.T) {
	for _, graph := range []bool{true, false} {
		for _, login := range []string{"first-account", "second-account"} {
			gh := func(_ context.Context, args ...string) ([]byte, error) {
				if args[1] == "graphql" {
					if !graph {
						return nil, errors.New("API rate limit already exceeded")
					}
					return json.Marshal(map[string]any{"data": map[string]any{"viewer": map[string]string{"login": login}}})
				}
				if graph {
					t.Fatal("login unexpectedly used REST")
				}
				return []byte(login), nil
			}
			got, err := ViewerLogin(context.Background(), gh)
			if err != nil || got != login {
				t.Fatalf("login=%q err=%v", got, err)
			}
		}
	}
}

func TestGraphQLDeskReviewVerdicts(t *testing.T) {
	for _, tc := range []struct {
		login, association, body string
		want                     int
	}{
		{"fixture-person", "NONE", "Review: changes requested", 1},
		{"fixture-person", "NONE", "Review: approved", 0},
		{"member", "MEMBER", "Review: changes requested", 1},
		{"outsider", "NONE", "Review: changes requested", 0},
	} {
		t.Run(tc.login+tc.body, func(t *testing.T) {
			gh := func(_ context.Context, args ...string) ([]byte, error) {
				if args[1] != "graphql" {
					t.Fatal("REST called")
				}
				q := args[len(args)-1]
				if strings.Contains(q, "viewer") {
					return []byte(graphReviewHead), nil
				}
				if strings.Contains(q, "items:reviews") {
					return reviewPage(fmt.Sprintf(`[{"id":"R1","user":{"login":%q,"type":"User"},"state":"COMMENTED","body":%q,"submitted_at":"2026-10-21T07:00:00Z","commit":{"oid":"old"},"author_association":%q,"comments":{"totalCount":0}}]`, tc.login, tc.body, tc.association), ""), nil
				}
				return reviewPage(`[]`, ""), nil
			}
			why, open, err := ReviewGuard(context.Background(), gh, "o/r", 7)
			if err != nil || open == nil || len(open) != tc.want || (why != "") != (tc.want != 0) {
				t.Fatalf("why=%q open=%v err=%v", why, open, err)
			}
		})
	}
}
