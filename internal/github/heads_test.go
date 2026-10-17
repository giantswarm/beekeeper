package github

import (
	"context"
	"strings"
	"testing"
)

func TestHeadPullsSkipsForks(t *testing.T) {
	run := func(_ context.Context, args ...string) ([]byte, error) {
		if q := args[len(args)-1]; !strings.Contains(q, `headRefName:"fix"`) || !strings.Contains(q, "isCrossRepository") {
			t.Errorf("query %s", q)
		}
		return []byte(`{"data":{
			"h0":{"pullRequests":{"nodes":[
				{"number":9,"state":"CLOSED","headRefOid":"f","closedAt":"2026-10-01T00:00:00Z","isCrossRepository":true},
				{"number":7,"state":"OPEN","headRefOid":"a","closedAt":null,"isCrossRepository":false}]}},
			"h1":{"pullRequests":{"nodes":[
				{"number":3,"state":"MERGED","headRefOid":"f","closedAt":"2026-10-01T00:00:00Z","isCrossRepository":true}]}}}}`), nil
	}
	fix, other := Head{Repo: "o/r", Branch: "fix"}, Head{Repo: "o/r", Branch: "other"}
	pulls, err := HeadPulls(context.Background(), run, []Head{fix, other})
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := pulls[fix]; !ok || p.N != 7 || p.State != Open {
		t.Errorf("fix: %+v, %v; want the repository's own open #7", p, ok)
	}
	if p, ok := pulls[other]; ok {
		t.Errorf("other: %+v; want none, its only pull request is a fork's", p)
	}
}
