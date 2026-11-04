package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const (
	headsRepo   = "o/r"
	headsNoData = `{"data":{}}`
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
	fix, other := Head{Repo: headsRepo, Branch: "fix"}, Head{Repo: headsRepo, Branch: "other"}
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

// A partial answer must never be interpreted as evidence that the other
// branch has no pull request.
func TestHeadPullsRejectsUnavailableAnswers(t *testing.T) {
	commandErr := errors.New("API failed")
	for _, tc := range []struct {
		name, response string
		err            error
	}{
		{"command with data", `{"data":{"h0":{"pullRequests":{"nodes":[]}}}}`, commandErr},
		{"invalid JSON", `{"data":{"h0":{"pullRequests":{"nodes":[]}}}} trailing`, nil},
		{"missing data", `{}`, nil},
		{"null data", `{"data":null}`, nil},
		{"missing alias", headsNoData, nil},
		{"null alias", `{"data":{"h0":null}}`, nil},
		{"invalid alias", `{"data":{"h0":[]}}`, nil},
		{"missing pulls", `{"data":{"h0":{}}}`, nil},
		{"null pulls", `{"data":{"h0":{"pullRequests":null}}}`, nil},
		{"missing nodes", `{"data":{"h0":{"pullRequests":{}}}}`, nil},
		{"null nodes", `{"data":{"h0":{"pullRequests":{"nodes":null}}}}`, nil},
		{"invalid nodes", `{"data":{"h0":{"pullRequests":{"nodes":{}}}}}`, nil},
		{"invalid pull", `{"data":{"h0":{"pullRequests":{"nodes":[{"number":"invalid"}]}}}}`, nil},
		{"partial batch", `{"data":{"h0":{"pullRequests":{"nodes":[{"number":1,"state":"OPEN"}]}},"h1":null}}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(context.Context, ...string) ([]byte, error) { return []byte(tc.response), tc.err }
			heads := []Head{{Repo: headsRepo, Branch: "one"}}
			if tc.name == "partial batch" {
				heads = append(heads, Head{Repo: headsRepo, Branch: "two"})
			}
			pulls, err := HeadPulls(context.Background(), run, heads)
			if err == nil || pulls != nil {
				t.Fatalf("got %v, %v; want no results and an error", pulls, err)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("lost command error: %v", err)
			}
		})
	}
}

func TestHeadPullsEmptyNodes(t *testing.T) {
	run := func(context.Context, ...string) ([]byte, error) {
		return []byte(`{"data":{"h0":{"pullRequests":{"nodes":[]}}}}`), nil
	}
	pulls, err := HeadPulls(context.Background(), run, []Head{{Repo: headsRepo, Branch: "one"}})
	if err != nil || len(pulls) != 0 {
		t.Fatalf("got %v, %v; want no pull requests", pulls, err)
	}
}

func TestHeadPullsDiscardsEarlierBatchesOnFailure(t *testing.T) {
	var heads []Head
	for i := 0; i < headsPerRequest+1; i++ {
		heads = append(heads, Head{Repo: headsRepo, Branch: fmt.Sprint(i)})
	}
	calls := 0
	run := func(context.Context, ...string) ([]byte, error) {
		calls++
		if calls > 1 {
			return []byte(headsNoData), nil
		}
		var response strings.Builder
		response.WriteString(`{"data":{`)
		for i := 0; i < headsPerRequest; i++ {
			if i > 0 {
				response.WriteString(",")
			}
			fmt.Fprintf(&response, `"h%d":{"pullRequests":{"nodes":[{"number":1,"state":"OPEN"}]}}`, i)
		}
		response.WriteString("}}")
		return []byte(response.String()), nil
	}
	pulls, err := HeadPulls(context.Background(), run, heads)
	if calls != 2 || err == nil || pulls != nil {
		t.Fatalf("calls=%d, pulls=%v, err=%v", calls, pulls, err)
	}
}
