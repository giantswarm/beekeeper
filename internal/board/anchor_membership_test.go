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

const (
	crossOwnerAnchor = "org/engine#9"
	rankAfterNine    = `I1 after "I9"`
)

func TestRankAnchorPreservesBlankCrossOwnerItems(t *testing.T) {
	var mutations []string
	c := &Client{Board: config.Board{Owner: personalOwner, Project: 7}}
	c.GH = func(_ context.Context, args ...string) ([]byte, error) {
		checkSelectionSchema(t, args)
		checkedQuery(t, args[3])
		q := args[3]
		switch {
		case strings.Contains(q, "fields(first:50)"):
			return []byte(metaJSON), nil
		case strings.Contains(q, "addProjectV2ItemById"):
			mutations = append(mutations, "add existing anchor")
			return []byte(`{"data":{"addProjectV2ItemById":{"item":{"id":"I9","project":{"id":"P"}}}}}`), nil
		case strings.HasPrefix(q, "query=mutation"):
			mutations = append(mutations, "delete existing anchor")
			return []byte(`{"data":{"deleteProjectV2Item":{"deletedItemId":"I9"}}}`), nil
		case strings.Contains(q, "node(id:$p)"):
			return []byte(`{"data":{"node":{"items":{"nodes":[{"id":"I9","project":{"id":"P"},"content":{"id":"ISSUE9"}}]}}}}`), nil
		case strings.Contains(q, "projectItems"):
			return []byte(`{"data":{"repository":{"issue":{"id":"ISSUE9","projectItems":{"nodes":[]}}}}}`), nil
		}
		return nil, fmt.Errorf("unexpected query: %s", q)
	}
	got, err := c.RankAnchor(t.Context(), engineIssue7, crossOwnerAnchor)
	if err != nil || got != crossOwnerAnchor || len(mutations) != 0 {
		t.Fatalf("blank existing anchor = %q, %v; mutations %v", got, err, mutations)
	}
}

func TestRankChecksCrossOwnerMembershipWithoutWriting(t *testing.T) {
	for _, tc := range []struct {
		name, page string
		wantError  bool
	}{
		{"blank existing item", `{"data":{"node":{"items":{"nodes":[{"id":"I9","content":{"id":"ISSUE9"}}]}}}}`, false},
		{"absent item", `{"data":{"node":{"items":{"nodes":[]}}}}`, true},
		{"unread membership", `{"data":{"node":null},"errors":[{"message":"unavailable"}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGH{t: t, item: onBoard, byIssue: map[string]string{"9": `{"data":{"repository":{"issue":{"id":"ISSUE9","projectItems":{"nodes":[]}}}}}`}}
			pages := 0
			c := &Client{Board: config.Board{Owner: personalOwner, Project: 7}, GH: func(ctx context.Context, args ...string) ([]byte, error) {
				if strings.Contains(args[3], "node(id:$p)") {
					checkSelectionSchema(t, args)
					pages++
					if slices.Contains(args, "c=next") {
						return []byte(tc.page), nil
					}
					return []byte(`{"data":{"node":{"items":{"nodes":[{"id":"D","content":null}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}}`), nil
				}
				return f.run(ctx, args...)
			}}
			rk, err := c.Rank(t.Context(), issue5, Place{After: crossOwnerAnchor})
			if (err != nil) != tc.wantError || pages != 2 {
				t.Fatalf("rank=%+v, error=%v, membership pages=%d", rk, err, pages)
			}
			if tc.wantError {
				if len(f.mutations) != 0 {
					t.Fatalf("refused rank wrote %v", f.mutations)
				}
			} else if !slices.Equal(f.mutations, []string{rankAfterNine}) {
				t.Fatalf("rank wrote %v", f.mutations)
			}
		})
	}
}

func TestCrossOwnerMembershipReadPropagatesCancellation(t *testing.T) {
	c := &Client{GH: func(context.Context, ...string) ([]byte, error) { return nil, context.Canceled }}
	if _, err := c.findItem(t.Context(), "P", "ISSUE9"); !errors.Is(err, context.Canceled) {
		t.Fatalf("membership cancellation: %v", err)
	}
}
