package board

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestReadFindsOpenSubIssuesBeyondTheFirstPage(t *testing.T) {
	f := &fakeGH{t: t, pages: []string{
		`{"data":{"repositoryOwner":{"projectV2":{"items":{"nodes":[` + node(inProgress, `{"name":"Epic 🎯"}`, issue(2, `,"subIssuesSummary":{"total":51,"completed":50}`)) + `]}}}}}`,
	}}
	f.extrasBy = func(q string) string {
		if strings.Contains(q, `after:"next"`) {
			return `{"data":{"e0":{"issue":{"subIssues":{"nodes":[` + issue(20, "") + `],"pageInfo":{"hasNextPage":false}}}}}}`
		}
		var closed []string
		for n := range 50 {
			closed = append(closed, strings.Replace(issue(n+100, ""), `"OPEN"`, `"CLOSED"`, 1))
		}
		return `{"data":{"e0":{"issue":{"subIssues":{"nodes":[` + strings.Join(closed, ",") + `],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}}}`
	}
	b := readBoard()
	snap, err := (&Client{GH: f.run, Board: b}).Read(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	got := Rank(snap, b, now)
	if len(got) != 1 || got[0].Ref != partialSubIssue || got[0].Skip != "" {
		t.Fatalf("open child after 50 closed children: %+v", got)
	}
}

func TestSubIssueContinuationKeepsOrderAndPartialEvidence(t *testing.T) {
	f := &fakeGH{t: t, pages: []string{
		`{"data":{"repositoryOwner":{"projectV2":{"items":{"nodes":[` + node(inProgress, `{"name":"Epic 🎯"}`, issue(2, `,"subIssuesSummary":{"total":51,"completed":49}`)) + `]}}}}}`,
	}}
	f.extrasBy = func(q string) string {
		if strings.Contains(q, `after:"next"`) {
			return `{"data":{"e0":{"issue":{"subIssues":{"nodes":[` + issue(20, `,"projectItems":null`) + `],"pageInfo":{"hasNextPage":false}}}}},"errors":[{"path":["e0","issue","subIssues","nodes",0,"projectItems"],"message":"` + limits + `"}]}`
		}
		return `{"data":{"e0":{"issue":{"subIssues":{"nodes":[` + issue(21, "") + `],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}}}`
	}
	b := readBoard()
	snap, err := (&Client{GH: f.run, Board: b}).Read(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	got := Rank(snap, b, now)
	if len(got) != 2 || got[0].Ref != "o/r#21" || got[0].Skip != "" || got[1].Ref != partialSubIssue || !strings.Contains(got[1].Skip, limits) {
		t.Fatalf("continued children: %+v", got)
	}
}

func TestSubIssueContinuationPropagatesCancellation(t *testing.T) {
	f := &fakeGH{t: t, pages: []string{
		`{"data":{"repositoryOwner":{"projectV2":{"items":{"nodes":[` + node(inProgress, `{"name":"Epic 🎯"}`, issue(2, `,"subIssuesSummary":{"total":51,"completed":50}`)) + `]}}}}}`,
	}, extrasBy: func(string) string {
		return `{"data":{"e0":{"issue":{"subIssues":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}}}`
	}}
	c := &Client{Board: readBoard(), GH: func(ctx context.Context, args ...string) ([]byte, error) {
		if strings.Contains(args[3], `after:"next"`) {
			return nil, context.Canceled
		}
		return f.run(ctx, args...)
	}}
	if _, err := c.Read(t.Context(), now); !errors.Is(err, context.Canceled) {
		t.Fatalf("continuation cancellation: %v", err)
	}
}
