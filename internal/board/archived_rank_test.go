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

// GitHub can report archived membership while omitting the item from board order.
func TestRankFallsBackWhenArchivedAnchorDisappears(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pages []string
		want  string
	}{
		{"live tail", []string{`{"nodes":[{"id":"I2"},{"id":"I3"},{"id":"I8","isArchived":true},{"id":"I1"}]}`}, "I3"},
		{"empty board", []string{`{"nodes":[]}`}, ""},
		{"no other live item", []string{`{"nodes":[{"id":"I8","isArchived":true},{"id":"I1"}]}`}, ""},
		{"paginated tail", []string{`{"nodes":[{"id":"I2"}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}`, `{"nodes":[{"id":"I3"},{"id":"I1"}]}`}, "I3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGH{t: t, item: onBoard, byIssue: map[string]string{"9": strings.Replace(item9, `"id":"I9"`, `"id":"I9","isArchived":true`, 1)}}
			reads := 0
			c := &Client{Board: config.Board{Owner: "o", Project: 7, Team: "X"}}
			c.GH = func(ctx context.Context, args ...string) ([]byte, error) {
				if strings.Contains(args[3], "node(id:$p)") {
					checkSelectionSchema(t, args)
					if reads >= len(tc.pages) {
						t.Fatal("unexpected board page read")
					}
					page := tc.pages[reads]
					reads++
					return []byte(`{"data":{"node":{"items":` + page + `}}}`), nil
				}
				return f.run(ctx, args...)
			}
			if _, err := c.Rank(context.Background(), issue5, Place{After: issue9}); err != nil {
				t.Fatal(err)
			}
			if reads != len(tc.pages) || !slices.Equal(f.mutations, []string{fmt.Sprintf("I1 after %q", tc.want)}) {
				t.Fatalf("mutations %v; reads %d; want after %q and %d reads", f.mutations, reads, tc.want, len(tc.pages))
			}
		})
	}
}
