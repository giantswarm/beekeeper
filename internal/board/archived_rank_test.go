package board

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
)

func TestRankSkipsArchivedAnchor(t *testing.T) {
	for _, predecessor := range []string{"I2", "", "I3"} {
		t.Run("predecessor="+predecessor, func(t *testing.T) {
			f := &fakeGH{t: t, item: onBoard, byIssue: map[string]string{"9": strings.Replace(item9, `"id":"I9"`, `"id":"I9","isArchived":true`, 1)}}
			reads := 0
			c := &Client{Board: config.Board{Owner: "o", Project: 7, Team: "X"}}
			c.GH = func(ctx context.Context, args ...string) ([]byte, error) {
				if strings.Contains(args[3], "node(id:$p)") {
					reads++
					checkSelectionSchema(t, args)
					if predecessor == "I3" {
						return []byte(`{"data":{"node":{"items":{"nodes":[{"id":"I1"},{"id":"I9","isArchived":true},{"id":"I3"}]}}}}`), nil
					}
					if predecessor == "" {
						return []byte(`{"data":{"node":{"items":{"nodes":[{"id":"I9","isArchived":true}]}}}}`), nil
					}
					if slices.Contains(args, "c=next") {
						return []byte(`{"data":{"node":{"items":{"nodes":[{"id":"I8","isArchived":true},{"id":"I1"},{"id":"I9","isArchived":true}]}}}}`), nil
					}
					return []byte(fmt.Sprintf(`{"data":{"node":{"items":{"nodes":[{"id":%q}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}}`, predecessor)), nil
				}
				return f.run(ctx, args...)
			}
			if _, err := c.Rank(context.Background(), issue5, Place{After: issue9}); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(f.mutations, []string{fmt.Sprintf("I1 after %q", predecessor)}) {
				t.Fatalf("mutations %v; reads %d", f.mutations, reads)
			}
		})
	}
}
