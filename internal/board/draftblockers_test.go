package board

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestReadSkipsDraftAndUnmergedBlockersAndOffersMerged(t *testing.T) {
	for _, tc := range []struct {
		name, issueState, prState string
		draft                     bool
		want                      string
	}{
		{"draft PR", stateOpen, stateOpen, true, "o/r#9 awaiting o/r#10 (draft)"},
		{"unmerged", stateOpen, stateOpen, false, "o/r#9 awaiting o/r#10 (unmerged)"},
		{"merged PR", "CLOSED", "MERGED", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := fmt.Sprintf(`,"blockedBy":{"totalCount":1,"nodes":[{"number":9,"state":%q,"repository":{"nameWithOwner":"o/r"}}]}`, tc.issueState)
			f := &fakeGH{t: t, pages: []string{`{"data":{"repositoryOwner":{"projectV2":{"items":{"nodes":[` + node(upNext, "null", issue(4, deps)) + `]}}}}}`}}
			b := readBoard()
			b.Order = append(b.Order[:2], b.Order[3:]...)
			run := func(ctx context.Context, args ...string) ([]byte, error) {
				if strings.Contains(strings.Join(args, " "), "closedByPullRequestsReferences") {
					return []byte(fmt.Sprintf(`{"data":{"b0":{"issue":{"closedByPullRequestsReferences":{"nodes":[{"number":10,"state":%q,"isDraft":%t,"repository":{"nameWithOwner":"o/r"}}]}}}}}`, tc.prState, tc.draft)), nil
				}
				return f.run(ctx, args...)
			}
			snap, err := (&Client{GH: run, Board: b}).Read(context.Background(), now)
			if err != nil {
				t.Fatal(err)
			}
			candidates := Rank(snap, b, now)
			if len(candidates) != 1 {
				t.Fatalf("candidates: %+v", candidates)
			}
			c := candidates[0]
			if tc.want == "" {
				if c.Skip != "" {
					t.Fatalf("merged blocker skipped: %s", c.Skip)
				}
			} else {
				if !strings.Contains(c.Skip, tc.want) {
					t.Fatalf("skip %q does not name %q", c.Skip, tc.want)
				}
				if len(c.OpenBlockedBy) != 1 || c.OpenBlockedBy[0] != "o/r#9" {
					t.Fatalf("blocker identity changed: %v", c.OpenBlockedBy)
				}
			}
		})
	}
}

func TestBlockerDetailsSharesReadsAcrossCandidateSources(t *testing.T) {
	item := func() Item { return Item{OpenBlockers: 1, Blockers: 1, OpenBlockedBy: []string{"o/r#9"}} }
	parent := item()
	parent.SubIssues = []Item{item()}
	snap := &Snapshot{Items: []Item{parent}, Search: map[int][]Item{0: {item()}}}
	calls := 0
	c := &Client{GH: func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		q := strings.Join(args, " ")
		if strings.Count(q, "issue(number:9)") != 1 {
			t.Fatalf("duplicate blocker reads: %s", q)
		}
		return []byte(`{"data":{"b0":{"issue":{"closedByPullRequestsReferences":{"nodes":[{"number":10,"state":"OPEN","isDraft":true,"repository":{"nameWithOwner":"o/r"}}]}}}}}`), nil
	}}
	if err := c.blockerDetails(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("requests: %d", calls)
	}
	for _, it := range []Item{snap.Items[0], snap.Items[0].SubIssues[0], snap.Search[0][0]} {
		if !strings.Contains(it.HeldText(), "o/r#10 (draft)") {
			t.Fatalf("missing draft: %s", it.HeldText())
		}
	}
}
