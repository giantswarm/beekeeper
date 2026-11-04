package github

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestLookup(t *testing.T) {
	var query string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		query = args[len(args)-1]
		// GitHub answers one pull request and not the other: the request
		// fails, the data stays.
		return []byte(`{"data":{"p0":{"pullRequest":{"title":"feat: one"}},"p1":{"pullRequest":null},
			"d":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":9,"repository":{"nameWithOwner":"example/plans"}}]}}}`), errors.New("exit status 1")
	}
	pulls := []PR{{Repo: "example/tools", N: 1}, {Repo: "example/tools", N: 404}}
	titles, drafts, err := Lookup(context.Background(), run, pulls, []string{"example/plans"})
	if err != nil || titles[pulls[0]] != "feat: one" || titles[pulls[1]] != "" || len(drafts) != 1 || drafts[0] != (PR{Repo: "example/plans", N: 9}) {
		t.Errorf("Lookup = %v, %v, %v", titles, drafts, err)
	}
	for _, want := range []string{`p0:repository(owner:"example",name:"tools"){pullRequest(number:1)`, `repo:example/plans`, "draft:true"} {
		if !strings.Contains(query, want) {
			t.Errorf("the query has no %s: %s", want, query)
		}
	}
	if _, _, err := Lookup(context.Background(), func(context.Context, ...string) ([]byte, error) { return nil, errors.New("offline") }, pulls, nil); err == nil {
		t.Error("Lookup without an answer: no error")
	}
	if titles, drafts, err := Lookup(context.Background(), nil, nil, nil); titles != nil || drafts != nil || err != nil {
		t.Error("Lookup of nothing asks GitHub")
	}
}

func TestLookupResolvesAPersonalAccountRepository(t *testing.T) {
	var acting []string
	run := ownerLink(&acting, func(query string) string {
		if strings.Contains(query, `owner:"teemow"`) {
			return `{"data":{"p0":{"pullRequest":{"title":"fix: lab"}},"d":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":3,"repository":{"nameWithOwner":"teemow/plans"}}]}}}`
		}
		return `{"data":{"p0":{"pullRequest":{"title":"feat: beekeeper"}}}}`
	})
	pulls := []PR{{Repo: orgRepo, N: 1}, {Repo: personalRepo, N: 7}}
	titles, drafts, err := Lookup(context.Background(), run, pulls, []string{"teemow/plans"})
	if err != nil || titles[pulls[0]] != "feat: beekeeper" || titles[pulls[1]] != "fix: lab" || !slices.Equal(drafts, []PR{{Repo: "teemow/plans", N: 3}}) {
		t.Errorf("Lookup = %v, %v, %v", titles, drafts, err)
	}
	if !slices.Equal(acting, []string{orgRepo, personalRepo}) {
		t.Errorf("requests acted on %q, want one per owner", acting)
	}
}

func TestLookupReadsEveryDraftPage(t *testing.T) {
	pull := PR{Repo: "example/tools", N: 1}
	var want []PR
	for n := 1; n <= 51; n++ {
		repo := "example/plans"
		if n == 51 {
			repo = "example/other"
		}
		want = append(want, PR{Repo: repo, N: n})
	}
	calls := 0
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		calls++
		if Repo(ctx) != pull.Repo {
			t.Errorf("request acted on %q", Repo(ctx))
		}
		query := args[len(args)-1]
		for _, fragment := range []string{"sort:created-asc", "repo:example/plans", "repo:example/other", "issueCount", "pageInfo{hasNextPage endCursor}"} {
			if !strings.Contains(query, fragment) {
				t.Errorf("query missing %q: %s", fragment, query)
			}
		}
		if calls == 1 {
			if !strings.Contains(query, "p0:repository") || strings.Contains(query, "after:") {
				t.Fatalf("unexpected initial query: %s", query)
			}
			nodes := make([]map[string]any, 50)
			for i, p := range want[:50] {
				nodes[i] = map[string]any{"number": p.N, "repository": map[string]string{"nameWithOwner": p.Repo}}
			}
			b, err := json.Marshal(map[string]any{"data": map[string]any{
				"p0": map[string]any{"pullRequest": map[string]string{"title": "requested title"}},
				"d":  map[string]any{"issueCount": 51, "pageInfo": map[string]any{"hasNextPage": true, "endCursor": "cursor-50"}, "nodes": nodes},
			}})
			return b, err
		}
		if calls != 2 || !strings.Contains(query, `after:"cursor-50"`) || strings.Contains(query, "repository(owner:") {
			t.Fatalf("unexpected continuation query: %s", query)
		}
		return []byte(`{"data":{"d":{"issueCount":51,"pageInfo":{"hasNextPage":false},"nodes":[{"number":51,"repository":{"nameWithOwner":"example/other"}}]}}}`), nil
	}
	titles, drafts, err := Lookup(context.Background(), run, []PR{pull}, []string{"example/plans", "example/other"})
	if err != nil || calls != 2 || titles[pull] != "requested title" || !slices.Equal(drafts, want) {
		t.Fatalf("Lookup = %v, %v, %v; calls = %d", titles, drafts, err, calls)
	}
}

func TestLookupDraftSearchFailures(t *testing.T) {
	offline := errors.New("synthetic read error")
	for _, tc := range []struct {
		name   string
		first  string
		second string
		err    error
	}{
		{"continuation", `{"issueCount":51,"pageInfo":{"hasNextPage":true,"endCursor":"next"}}`, `{"data":{"d":{"pageInfo":{"hasNextPage":false}}}}`, offline},
		{"continuation without data", `{"pageInfo":{"hasNextPage":true,"endCursor":"next"}}`, `{"data":null}`, offline},
		{"malformed continuation", `{"pageInfo":{"hasNextPage":true,"endCursor":"next"}}`, `invalid json`, nil},
		{"search cap", `{"issueCount":1001,"pageInfo":{"hasNextPage":false}}`, "", nil},
		{"short result", `{"issueCount":51,"pageInfo":{"hasNextPage":false}}`, "", nil},
		{"missing cursor", `{"pageInfo":{"hasNextPage":true}}`, "", nil},
		{"repeated cursor", `{"pageInfo":{"hasNextPage":true,"endCursor":"next"}}`, `{"data":{"d":{"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}`, nil},
		{"missing page info", `{"nodes":[]}`, "", nil},
		{"missing search", `null`, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			run := func(context.Context, ...string) ([]byte, error) {
				calls++
				if calls == 1 {
					return []byte(`{"data":{"d":` + tc.first + `}}`), nil
				}
				if calls > 2 || tc.second == "" {
					t.Fatal("unexpected continuation")
				}
				return []byte(tc.second), tc.err
			}
			_, _, err := Lookup(context.Background(), run, nil, []string{"example/plans"})
			if err == nil || (tc.err != nil && !errors.Is(err, tc.err)) {
				t.Fatalf("Lookup error = %v, want failure %v", err, tc.err)
			}
		})
	}
}
