package board

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
)

func TestRankAnchorPreservesBlankCrossOwnerItems(t *testing.T) {
	for _, present := range []bool{true, false} {
		t.Run(fmt.Sprintf("present=%t", present), func(t *testing.T) {
			reads := 0
			c := &Client{Board: config.Board{Owner: "personal", Project: 1}}
			c.GH = func(_ context.Context, args ...string) ([]byte, error) {
				checkSelectionSchema(t, args)
				q := args[3]
				switch {
				case strings.Contains(q, "mutation"):
					t.Fatalf("anchor lookup mutated membership: %s", q)
				case strings.Contains(q, "fields(first:50)"):
					return []byte(metaJSON), nil
				case strings.Contains(q, "node(id:$p)"):
					reads++
					if !slices.Contains(args, "c=next") {
						return []byte(`{"data":{"node":{"items":{"nodes":[{"id":"OTHER","content":{"id":"OTHER"}}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}}`), nil
					}
					if present {
						return []byte(`{"data":{"node":{"items":{"nodes":[{"id":"I9","project":{"id":"P"},"status":null,"team":null,"kind":null,"content":{"id":"ISSUE9"}}]}}}}`), nil
					}
					return []byte(`{"data":{"node":{"items":{"nodes":[]}}}}`), nil
				case strings.Contains(q, "projectItems"):
					id := "NEW"
					if slices.Contains(args, "n=9") {
						id = "ISSUE9"
					}
					return []byte(fmt.Sprintf(`{"data":{"repository":{"issue":{"id":%q,"projectItems":{"nodes":[]}}}}}`, id)), nil
				}
				return nil, fmt.Errorf("unexpected query: %s", q)
			}
			got, err := c.RankAnchor(context.Background(), "org/engine#51", "org/engine#9")
			want := ""
			if present {
				want = "org/engine#9"
			}
			if err != nil || got != want || reads != 2 {
				t.Fatalf("anchor = %q, %v; reads=%d; want %q", got, err, reads, want)
			}
		})
	}
}
