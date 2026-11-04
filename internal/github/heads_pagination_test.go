package github

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const (
	headPaginationRepo   = "o/r"
	headPaginationBranch = "fix"
)

func TestHeadPullsReadsPastForks(t *testing.T) {
	var forks []headNode
	for i := range pullsPerHead {
		forks = append(forks, headNode{Number: 100 + i, State: Closed, Fork: true})
	}
	for _, tc := range []struct {
		name    string
		last    []headNode
		failure error
	}{
		{"case 0: repository PR after forks", []headNode{{Number: 7, State: Open, HeadOid: "own"}, {Number: 6, State: Closed}}, nil},
		{"case 1: forks only", []headNode{{Number: 99, State: Open, Fork: true}}, nil},
		{"case 2: failed continuation", nil, errors.New("pull request continuation unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			const cursor = "after-forks"
			other := Head{Repo: headPaginationRepo, Branch: "other"}
			run := func(ctx context.Context, args ...string) ([]byte, error) {
				calls++
				if Repo(ctx) != headPaginationRepo {
					t.Fatalf("request acted on %q", Repo(ctx))
				}
				q := args[len(args)-1]
				page := func(nodes []headNode, next bool) any {
					return map[string]any{"pullRequests": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": cursor}}}
				}
				data := map[string]any{}
				switch calls {
				case 1:
					data["h0"] = page(forks, true)
					data["h1"] = page([]headNode{{Number: 8, State: Merged}}, true)
				case 2:
					if !strings.Contains(q, `after:"`+cursor+`"`) || strings.Contains(q, `headRefName:"other"`) {
						t.Fatalf("wrong continuation %s", q)
					}
					if tc.failure != nil {
						return nil, tc.failure
					}
					data["h0"] = page(tc.last, false)
				default:
					t.Fatal("unexpected extra request")
				}
				return json.Marshal(map[string]any{"data": data})
			}
			head := Head{Repo: headPaginationRepo, Branch: headPaginationBranch}
			got, err := HeadPulls(t.Context(), run, []Head{head, other})
			if tc.failure != nil {
				if !errors.Is(err, tc.failure) {
					t.Fatalf("error = %v, want continuation failure", err)
				}
				return
			}
			if err != nil || calls != 2 {
				t.Fatalf("calls = %d, error %v", calls, err)
			}
			if got[other].N != 8 {
				t.Fatal("lost first-page PR")
			}
			if tc.last[0].Fork {
				if _, ok := got[head]; ok {
					t.Fatal("fork used as repository PR")
				}
			} else if p := got[head]; p.N != 7 || p.State != Open || p.HeadOid != "own" {
				t.Fatalf("repository PR = %+v, want open #7", p)
			}
		})
	}
}

func TestHeadPullsStopsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	run := func(context.Context, ...string) ([]byte, error) {
		calls++
		if calls > 1 {
			t.Fatal("read another page after cancellation")
		}
		cancel()
		return []byte(`{"data":{"h0":{"pullRequests":{"nodes":[{"number":9,"isCrossRepository":true}],"pageInfo":{"hasNextPage":true,"endCursor":"forks"}}}}}`), nil
	}
	if _, err := HeadPulls(ctx, run, []Head{{Repo: headPaginationRepo, Branch: headPaginationBranch}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
}

func TestHeadPullsRejectsInvalidPagination(t *testing.T) {
	for _, cursor := range []string{"", "unchanged"} {
		t.Run("cursor="+cursor, func(t *testing.T) {
			calls := 0
			run := func(context.Context, ...string) ([]byte, error) {
				calls++
				if calls > 2 {
					t.Fatal("pagination did not terminate")
				}
				return json.Marshal(map[string]any{"data": map[string]any{"h0": map[string]any{
					"pullRequests": map[string]any{"nodes": []headNode{{Number: 9, Fork: true}},
						"pageInfo": map[string]any{"hasNextPage": true, "endCursor": cursor}},
				}}})
			}
			if _, err := HeadPulls(t.Context(), run, []Head{{Repo: headPaginationRepo, Branch: headPaginationBranch}}); err == nil {
				t.Fatal("invalid continuation reported as no repository PR")
			}
		})
	}
}
