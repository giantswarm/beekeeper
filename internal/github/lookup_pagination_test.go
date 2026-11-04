package github

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestLookupReadsEveryDraftPage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
	}{
		{"case 0: draft after first 50", nil},
		{"case 1: failed draft continuation", errors.New("draft continuation unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			const repo = "example/plans"
			run := func(ctx context.Context, args ...string) ([]byte, error) {
				calls++
				if Repo(ctx) != repo {
					t.Fatalf("request acted on %q", Repo(ctx))
				}
				q := args[len(args)-1]
				var nodes []map[string]any
				next := false
				data := map[string]any{}
				switch calls {
				case 1:
					for i := range 50 {
						nodes = append(nodes, map[string]any{"number": i + 1, "repository": map[string]string{"nameWithOwner": repo}})
					}
					next = true
					data["p0"] = map[string]any{"pullRequest": map[string]string{"title": "merged change"}}
				case 2:
					if !strings.Contains(q, `after:"draft-page-1"`) || strings.Contains(q, "pullRequest(number:") {
						t.Fatalf("wrong continuation %s", q)
					}
					if tc.failure != nil {
						return nil, tc.failure
					}
					nodes = append(nodes, map[string]any{"number": 51, "repository": map[string]string{"nameWithOwner": repo}})
				default:
					t.Fatal("unexpected extra request")
				}
				data["d"] = map[string]any{"nodes": nodes, "issueCount": 51, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": "draft-page-1"}}
				return json.Marshal(map[string]any{"data": data})
			}
			pull := PR{Repo: repo, N: 99}
			titles, drafts, err := Lookup(t.Context(), run, []PR{pull}, []string{repo})
			if tc.failure != nil {
				if !errors.Is(err, tc.failure) {
					t.Fatalf("error = %v, want continuation failure", err)
				}
				return
			}
			if err != nil || calls != 2 || len(drafts) != 51 || !slices.Contains(drafts, PR{Repo: repo, N: 51}) || titles[pull] != "merged change" {
				t.Fatalf("drafts %d, calls %d, title %q, error %v", len(drafts), calls, titles[pull], err)
			}
		})
	}
}

func TestLookupRejectsIncompleteDraftSearch(t *testing.T) {
	for name, search := range map[string]string{
		"no search data":               "null",
		"search ends before its count": `{"issueCount":51,"nodes":[],"pageInfo":{"hasNextPage":false}}`,
		"missing cursor":               `{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":""}}`,
		"nonadvancing cursor":          `{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"unchanged"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			run := func(context.Context, ...string) ([]byte, error) {
				calls++
				if calls > 2 {
					t.Fatal("draft pagination did not terminate")
				}
				return []byte(`{"data":{"d":` + search + `}}`), nil
			}
			if _, _, err := Lookup(t.Context(), run, nil, []string{"example/plans"}); err == nil {
				t.Fatal("incomplete search returned as complete")
			}
		})
	}
}

func TestLookupStopsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	run := func(context.Context, ...string) ([]byte, error) {
		calls++
		if calls > 1 {
			t.Fatal("draft read continued after cancellation")
		}
		cancel()
		return []byte(`{"data":{"d":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}`), nil
	}
	if _, _, err := Lookup(ctx, run, nil, []string{"example/plans"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
}
