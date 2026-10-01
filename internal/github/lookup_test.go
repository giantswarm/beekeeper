package github

import (
	"context"
	"errors"
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
			"d":{"nodes":[{"number":9,"repository":{"nameWithOwner":"example/plans"}}]}}}`), errors.New("exit status 1")
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
