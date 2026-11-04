package board

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestReadFindsOpenSubIssuesBeyondTheFirstPage(t *testing.T) {
	for _, mode := range []string{"closed first page", "ordered partial pages", "cancellation"} {
		t.Run(mode, func(t *testing.T) {
			b := &largeBoard{t: t, progress: 1, subs: 51, open: 1}
			board := b.order()
			board.Order = board.Order[:2]
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			pages := 0
			gh := func(ctx context.Context, args ...string) ([]byte, error) {
				q := strings.TrimPrefix(args[3], "query=")
				if !strings.Contains(q, "subIssues(") {
					return b.run(ctx, args...)
				}
				pages++
				if !strings.Contains(q, "pageInfo{hasNextPage endCursor}") {
					t.Error("missing child pageInfo")
				}
				if pages == 1 {
					if !strings.Contains(q, "subIssues(first:50)") {
						t.Fatalf("unbounded first page: %s", q)
					}
					var children []string
					for n := range 50 {
						children = append(children, strings.Replace(issue(100+n, ""), "OPEN", "CLOSED", 1))
					}
					if mode == "ordered partial pages" {
						children[0] = issue(100, "")
					}
					if mode == "cancellation" {
						cancel()
					}
					return fmt.Appendf(nil, `{"data":{"e0":{"issue":{"subIssues":{"pageInfo":{"hasNextPage":true,"endCursor":"child-page-1"},"nodes":[%s]}}}}}`, strings.Join(children, ",")), nil
				}
				if pages != 2 {
					t.Fatalf("unexpected child page %d", pages)
				}
				if !strings.Contains(q, `subIssues(first:50,after:"child-page-1")`) {
					t.Fatalf("missing bounded continuation: %s", q)
				}
				errs := ""
				if mode == "ordered partial pages" {
					errs = `,"errors":[{"path":["e0","issue","subIssues","nodes",0,"projectItems"],"message":"unavailable"}]`
				}
				children := issue(150, "")
				if mode == "ordered partial pages" {
					children += "," + issue(151, "")
				}
				return []byte(`{"data":{"e0":{"issue":{"subIssues":{"pageInfo":{"hasNextPage":false},"nodes":[` + children + `]}}}}` + errs + `}`), nil
			}
			snap, err := (&Client{GH: gh, Board: board}).Read(ctx, now)
			if mode == "cancellation" {
				if !errors.Is(err, context.Canceled) || pages != 1 {
					t.Fatalf("Read error=%v, pages=%d", err, pages)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			cands := Rank(snap, board, now)
			var got []string
			for _, c := range cands {
				got = append(got, c.Ref)
			}
			want := []string{"o/r#150"}
			if mode == "ordered partial pages" {
				want = []string{"o/r#100", "o/r#150", "o/r#151"}
				if !slices.Equal(cands[1].Missing, []string{"projectItems: unavailable"}) || cands[1].Skip == "" || len(cands[0].Missing) != 0 || len(cands[2].Missing) != 0 {
					t.Fatalf("partial errors attributed incorrectly: %+v", cands)
				}
			}
			if !slices.Equal(got, want) || pages != 2 {
				t.Fatalf("candidates=%v, want %v; pages=%d", got, want, pages)
			}
		})
	}
}
