package board

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
)

func TestReadRetainsLeaseLabelsPastTheFirstPage(t *testing.T) {
	for _, mode := range []string{"complete", "late selection", "small", "transport error", "partial error", "missing issue", "missing cursor", "stalled cursor", "initial partial error"} {
		t.Run(mode, func(t *testing.T) {
			var labels []string
			for i := 0; i < 20; i++ {
				labels = append(labels, fmt.Sprintf(`{"name":"ordinary-%d"}`, i))
			}
			pageInfo := `,"pageInfo":{"hasNextPage":true,"endCursor":"L1"}`
			if mode == "small" {
				pageInfo = ""
			}
			extra := `,"labels":{"nodes":[` + strings.Join(labels, ",") + `]` + pageInfo + `}`
			f := &fakeGH{t: t, pages: []string{`{"data":{"repositoryOwner":{"projectV2":{"items":{"nodes":[` +
				node(inProgress, `{"name":"Epic 🎯"}`, issue(2, `,"subIssuesSummary":{"total":1,"completed":0}`)) + "," +
				node(upNext, "null", issue(4, extra)) + `]}}}}}`}, extras: `{"data":{"e0":{"issue":{"subIssues":{"nodes":[` + issue(20, extra) + `]}}},"s2":{"nodes":[` + issue(30, extra) + `]}}}`}
			f.extrasBy = func(string) string { return f.extras }
			b := readBoard()
			if mode == "late selection" {
				// A late label must be available before deciding to read sub-issues.
				b.Order[0].Labels = []string{"ready"}
				f.pages[0] = strings.Replace(f.pages[0], issue(2, `,"subIssuesSummary":{"total":1,"completed":0}`), issue(2, `,"subIssuesSummary":{"total":1,"completed":0}`+extra), 1)
			}
			calls := 0
			c := &Client{Board: b, GH: func(ctx context.Context, args ...string) ([]byte, error) {
				q := strings.Join(args, " ")
				if !strings.Contains(q, "labels(first:100,after:$c)") {
					return f.run(ctx, args...)
				}
				calls++
				if !strings.Contains(q, "c=L1") && !strings.Contains(q, "c=L2") {
					t.Errorf("missing label cursor")
				}
				switch mode {
				case "transport error":
					return nil, errors.New("synthetic failure")
				case "partial error":
					return []byte(`{"data":{"repository":{"issue":{"labels":null}}},"errors":[{"path":["repository","issue","labels"],"message":"synthetic failure"}]}`), nil
				case "missing issue":
					return []byte(`{"data":{"repository":{"issue":null}}}`), nil
				case "missing cursor":
					return []byte(`{"data":{"repository":{"issue":{"labels":{"pageInfo":{"hasNextPage":true}}}}}}`), nil
				case "stalled cursor":
					return []byte(`{"data":{"repository":{"issue":{"labels":{"pageInfo":{"hasNextPage":true,"endCursor":"L1"}}}}}}`), nil
				}
				if strings.Contains(q, "c=L1") {
					return []byte(`{"data":{"repository":{"issue":{"labels":{"nodes":[{"name":"lease/offline-lab"}],"pageInfo":{"hasNextPage":true,"endCursor":"L2"}}}}}}`), nil
				}
				return []byte(`{"data":{"repository":{"issue":{"labels":{"nodes":[{"name":"kind/bug"},{"name":"ready"}],"pageInfo":{"hasNextPage":false}}}}}}`), nil
			}}
			if mode == "initial partial error" {
				f.extrasBy = func(string) string {
					return strings.TrimSuffix(f.extras, "}") + `,"errors":[{"path":["s2","nodes",0,"labels"],"message":"synthetic failure"}]}`
				}
			}
			snap, err := c.Read(t.Context(), now)
			if mode != "complete" && mode != "late selection" && mode != "small" {
				if err == nil {
					t.Fatal("unread labels accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]Candidate{}
			for _, cand := range Rank(snap, c.Board, now) {
				got[cand.Ref] = cand
			}
			for _, ref := range []string{ref4, partialSubIssue, "o/r#30"} {
				t.Run(ref, func(t *testing.T) {
					cand, ok := got[ref]
					if !ok {
						t.Fatal("missing candidate")
					}
					if mode == "small" {
						return
					}
					if !slices.Equal(cand.Leases(), []string{"offline-lab"}) {
						t.Errorf("leases: %v", cand.Leases())
					}
					if !Matches(config.BoardStep{Kind: []string{"Bug"}, Labels: []string{"ready"}}, cand.Item, now) {
						t.Error("late labels did not match configured kind and labels")
					}
					if !slices.Contains(cand.Labels, "kind/bug") || !slices.Contains(cand.Labels, "ready") {
						t.Errorf("labels: %v", cand.Labels)
					}
				})
			}
			want := 6
			if mode == "late selection" {
				want = 8
			}
			if mode == "small" {
				want = 0
			}
			if calls != want || len(f.queries) != 3 {
				t.Errorf("label calls=%d base calls=%d", calls, len(f.queries))
			}
		})
	}
}
