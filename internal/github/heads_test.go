package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestHeadPullsReadsPastForks(t *testing.T) {
	failure := errors.New("continuation unavailable")
	for _, tc := range []struct {
		name    string
		last    string
		err     error
		wantN   int
		wantErr bool
	}{
		{"own pull", `{"nodes":[{"number":7,"state":"OPEN","headRefOid":"a"},{"number":6,"state":"CLOSED"}],"pageInfo":{"hasNextPage":true,"endCursor":"unused"}}`, nil, 7, false},
		{"forks only", `{"nodes":[{"number":8,"isCrossRepository":true}],"pageInfo":{"hasNextPage":false}}`, nil, 0, false},
		{"failed continuation", "", failure, 0, true},
		{"missing continuation", "", nil, 0, true},
		{"null continuation", "null", nil, 0, true},
		{"invalid cursor", `{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"second"}}`, nil, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			run := func(_ context.Context, args ...string) ([]byte, error) {
				calls++
				q := args[len(args)-1]
				if !strings.Contains(q, "first:10") || !strings.Contains(q, "pageInfo{hasNextPage endCursor}") {
					t.Errorf("missing bounded pagination: %s", q)
				}
				switch calls {
				case 1:
					if strings.Contains(q, "after:") {
						t.Errorf("initial cursor: %s", q)
					}
					forks := make([]headNode, pullsPerHead)
					for i := range forks {
						forks[i] = headNode{Number: 100 - i, Fork: true}
					}
					nodes, _ := json.Marshal(forks)
					return []byte(fmt.Sprintf(`{"data":{"h0":{"pullRequests":{"nodes":%s,"pageInfo":{"hasNextPage":true,"endCursor":"first"}}},"h1":{"pullRequests":{"nodes":[{"number":3,"state":"MERGED","closedAt":"2026-10-01T00:00:00Z"}]}}}}`, nodes)), nil
				case 2, 3:
					cursor := "first"
					if calls == 3 {
						cursor = "second"
					}
					if !strings.Contains(q, "after:"+quote(cursor)) || strings.Contains(q, `headRefName:"other"`) || strings.Contains(q, "h1:") {
						t.Errorf("continuation rereads resolved branch or wrong cursor: %s", q)
					}
					if calls == 2 {
						return []byte(`{"data":{"h0":{"pullRequests":{"nodes":[{"isCrossRepository":true}],"pageInfo":{"hasNextPage":true,"endCursor":"second"}}}}}`), nil
					}
					if tc.last == "" {
						return []byte(`{"data":{}}`), tc.err
					}
					return []byte(`{"data":{"h0":{"pullRequests":` + tc.last + `}}}`), tc.err
				default:
					t.Fatal("unexpected extra request")
					return nil, nil
				}
			}
			fix, other := Head{Repo: "o/r", Branch: "fix"}, Head{Repo: "o/r", Branch: "other"}
			pulls, err := HeadPulls(context.Background(), run, []Head{fix, other})
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Errorf("lost error: %v", err)
			}
			if calls != 3 {
				t.Errorf("requests = %d, want 3", calls)
			}
			if !tc.wantErr {
				if pulls[fix].N != tc.wantN {
					t.Errorf("fix = %+v, want #%d", pulls[fix], tc.wantN)
				}
				if p := pulls[other]; p.N != 3 || p.ClosedAt.IsZero() {
					t.Errorf("other = %+v", p)
				}
			}
		})
	}
}
