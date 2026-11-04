package board

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const searchedFreeRef = "o/r#30"

func TestReadSearchFindsWorkBeyondFiftyAssignedIssues(t *testing.T) {
	var nodes []string
	for n := range 50 {
		nodes = append(nodes, issue(n+100, `,"assignees":{"nodes":[{"login":"pat"}]}`))
	}
	f := &fakeGH{t: t, pages: []string{`{"data":{"repositoryOwner":{"projectV2":{"items":{"nodes":[]}}}}}`}, extrasBy: func(string) string {
		return `{"data":{"s2":{"nodes":[` + strings.Join(nodes, ",") + `],"pageInfo":{"hasNextPage":true,"endCursor":"search-next"}}}}`
	}}
	c := &Client{Board: readBoard(), GH: func(ctx context.Context, args ...string) ([]byte, error) {
		if strings.Contains(args[3], "after:$c") && strings.Contains(args[3], "search(") {
			checkSelectionSchema(t, args)
			return []byte(`{"data":{"search":{"nodes":[` + issue(30, "") + `],"pageInfo":{"hasNextPage":false}}}}`), nil
		}
		return f.run(ctx, args...)
	}}
	snap, err := c.Read(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	got := Rank(snap, c.Board, now)
	if len(got) != 51 {
		t.Fatalf("search returned %d candidates; wanted 50 assigned issues then one free issue", len(got))
	}
	for i, cand := range got {
		if i < 50 && cand.Skip == "" {
			t.Errorf("assigned issue offered: %+v", cand)
		}
	}
	if last := got[len(got)-1]; last.Ref != searchedFreeRef || last.Skip != "" {
		t.Fatalf("next free search result: %+v", last)
	}
	if len(f.mutations) != 0 {
		t.Fatal(fmt.Sprint("search wrote: ", f.mutations))
	}
}

func TestSearchContinuationRetainsPartialBlockerEvidence(t *testing.T) {
	c := &Client{GH: func(_ context.Context, args ...string) ([]byte, error) {
		checkSelectionSchema(t, args)
		return []byte(`{"data":{"search":{"nodes":[` + issue(30, `,"blockedBy":{"totalCount":1,"nodes":[null]}`) + `],"pageInfo":{"hasNextPage":false}}},"errors":[{"path":["search","nodes",0,"blockedBy","nodes",0],"message":"` + limits + `"}]}`), nil
	}}
	items, missing, err := c.searchAfter(t.Context(), "repo:o/q", "next")
	if err != nil || len(items) != 1 || items[0].OpenBlockers != 1 || len(missing) != 1 || missing[0] != limits {
		t.Fatalf("partial search: items=%+v, missing=%v, error=%v", items, missing, err)
	}
}

func TestSearchContinuationPropagatesCancellation(t *testing.T) {
	c := &Client{GH: func(context.Context, ...string) ([]byte, error) { return nil, context.Canceled }}
	if _, _, err := c.searchAfter(t.Context(), "repo:o/q", "next"); !errors.Is(err, context.Canceled) {
		t.Fatalf("search cancellation: %v", err)
	}
}
