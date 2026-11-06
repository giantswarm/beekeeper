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
	var labels []string
	for n := range 20 {
		labels = append(labels, fmt.Sprintf(`{"name":"label-%d"}`, n))
	}
	extra := `,"labels":{"nodes":[` + strings.Join(labels, ",") + `],"pageInfo":{"hasNextPage":true,"endCursor":"labels-next"}}`
	f := &fakeGH{t: t, pages: []string{
		`{"data":{"repositoryOwner":{"projectV2":{"items":{"nodes":[` +
			node(inProgress, "null", issue(1, extra)) + "," +
			node(inProgress, `{"name":"Epic 🎯"}`, issue(2, `,"subIssuesSummary":{"total":1,"completed":0}`)) + `]}}}}}`,
	}, extrasBy: func(string) string {
		return `{"data":{"e1":{"issue":{"subIssues":{"nodes":[` + issue(20, extra) + `]}}},"s2":{"nodes":[` + issue(30, extra) + `]}}}`
	}}
	c := &Client{Board: readBoard(), GH: func(ctx context.Context, args ...string) ([]byte, error) {
		if strings.Contains(args[3], "labels(first:100,after:$c)") {
			checkSelectionSchema(t, args)
			if !slices.Contains(args, "c=labels-next") {
				t.Fatalf("missing label cursor: %v", args)
			}
			return []byte(`{"data":{"repository":{"issue":{"labels":{"nodes":[{"name":"lease/offline-lab"}],"pageInfo":{"hasNextPage":false}}}}}}`), nil
		}
		return f.run(ctx, args...)
	}}
	snap, err := c.Read(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	got := Rank(snap, c.Board, now)
	if len(got) != 3 {
		t.Fatalf("candidates: %+v", got)
	}
	for _, cand := range got {
		if !slices.Contains(cand.Leases(), "offline-lab") {
			t.Errorf("%s lost its required lease: %v", cand.Ref, cand.Labels)
		}
	}
}

func TestReadCompletesLabelsBeforeExpandingMatchedChildren(t *testing.T) {
	var labels []string
	for n := range 20 {
		labels = append(labels, fmt.Sprintf(`{"name":"label-%d"}`, n))
	}
	extra := `,"subIssuesSummary":{"total":1,"completed":0},"labels":{"nodes":[` + strings.Join(labels, ",") + `],"pageInfo":{"hasNextPage":true,"endCursor":"labels-next"}}`
	c := &Client{Board: config.Board{Owner: "o", Project: 7, Team: "X", Order: []config.BoardStep{{Name: "urgent children", Labels: []string{"urgent"}, SubIssues: true}}}}
	c.GH = func(_ context.Context, args ...string) ([]byte, error) {
		checkSelectionSchema(t, args)
		switch q := args[3]; {
		case strings.Contains(q, "fields(first:50)"):
			return []byte(metaJSON), nil
		case strings.Contains(q, boardItemsQuery):
			return []byte(`{"data":{"repositoryOwner":{"projectV2":{"items":{"nodes":[` + node(inProgress, "null", issue(2, extra)) + `]}}}}}`), nil
		case strings.Contains(q, "labels(first:100,after:$c)"):
			return []byte(`{"data":{"repository":{"issue":{"labels":{"nodes":[{"name":"urgent"}],"pageInfo":{"hasNextPage":false}}}}}}`), nil
		case strings.Contains(q, "subIssues(first:"):
			return []byte(`{"data":{"e0":{"issue":{"subIssues":{"nodes":[` + issue(20, "") + `]}}}}}`), nil
		}
		return nil, fmt.Errorf("unexpected query: %s", args[3])
	}
	snap, err := c.Read(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	got := Rank(snap, c.Board, now)
	if len(got) != 1 || got[0].Ref != partialSubIssue || got[0].Skip != "" {
		t.Fatalf("label-selected children: %+v", got)
	}
}

func TestLabelContinuationReportsUnreadLabels(t *testing.T) {
	for _, canceled := range []bool{true, false} {
		t.Run(fmt.Sprintf("canceled=%t", canceled), func(t *testing.T) {
			c := &Client{GH: func(context.Context, ...string) ([]byte, error) {
				if canceled {
					return nil, context.Canceled
				}
				return []byte(`{"data":{"repository":{"issue":{"labels":null}}},"errors":[{"path":["repository","issue","labels"],"message":"unread labels"}]}`), nil
			}}
			snap := &Snapshot{Items: []Item{{Ref: ref1, labelsMore: true, labelsCursor: "labels-continuation"}}}
			err := c.readLabels(t.Context(), snap)
			if err == nil || canceled && !errors.Is(err, context.Canceled) {
				t.Fatalf("unread continuation: %v", err)
			}
		})
	}
}
