package board

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
)

func TestReadRetainsAncestorsOutsideTheOrdersStatuses(t *testing.T) {
	c := &Client{Board: config.Board{Owner: "o", Project: 7, Order: []config.BoardStep{{Name: "work", Status: []string{inProgress}}}}}
	c.GH = func(_ context.Context, args ...string) ([]byte, error) {
		q := args[3]
		if strings.Contains(q, "fields(first:50)") {
			return []byte(largeMeta), nil
		}
		var nodes []string
		nodes = append(nodes, node(inProgress, "null", issue(20, `,"parent":{"number":2,"repository":{"nameWithOwner":"o/r"}}`)))
		if slices.Contains(args, "q=is:open") || strings.Contains(strings.Join(args, " "), "Inbox") {
			nodes = append(nodes, node("Inbox 📥", `{"name":"Epic 🎯"}`, issue(2, "")))
		}
		return []byte(`{"data":{"repositoryOwner":{"projectV2":{"items":{"nodes":[` + strings.Join(nodes, ",") + `]}}}}}`), nil
	}
	snap, err := c.Read(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	got := Rank(snap, c.Board, now)
	if len(got) != 1 || !strings.Contains(got[0].Skip, NotScheduled) {
		t.Fatalf("child of an Inbox epic: %+v", got)
	}
}

func TestReadPreservesAncestorHoldsWithANarrowOrder(t *testing.T) {
	for _, tc := range []struct {
		name, status, kind, extra, want string
	}{
		{"no epic status", "", `{"name":"Epic 🎯"}`, "", NotScheduled},
		{"blocked ancestor", blocked, "null", `,"blockedBy":{"totalCount":1,"nodes":[{"number":9,"state":"OPEN","repository":{"nameWithOwner":"o/r"}}]}`, HeldWord},
		{"blocker closed", blocked, "null", `,"blockedBy":{"totalCount":1,"nodes":[{"number":9,"state":"CLOSED","repository":{"nameWithOwner":"o/r"}}]}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{Board: config.Board{Owner: "o", Project: 7, Team: "X", Order: []config.BoardStep{{Name: "work", Status: []string{inProgress}}}}}
			c.GH = func(_ context.Context, args ...string) ([]byte, error) {
				checkSelectionSchema(t, args)
				if strings.Contains(args[3], "closedByPullRequestsReferences") {
					return []byte(`{"data":{}}`), nil
				}
				if strings.Contains(args[3], "fields(first:50)") {
					return []byte(largeMeta), nil
				}
				if !slices.Contains(args, `q=is:open team:"X"`) {
					return nil, fmt.Errorf("ancestor omitted by filter: %v", args)
				}
				return []byte(`{"data":{"repositoryOwner":{"projectV2":{"items":{"nodes":[` +
					node(tc.status, tc.kind, issue(2, tc.extra)) + "," +
					node(inProgress, "null", issue(20, `,"parent":{"number":2,"repository":{"nameWithOwner":"o/r"}}`)) + `]}}}}}`), nil
			}
			snap, err := c.Read(t.Context(), now)
			if err != nil {
				t.Fatal(err)
			}
			got := Rank(snap, c.Board, now)
			if len(got) != 1 || got[0].Ref != partialSubIssue || (tc.want == "" && got[0].Skip != "") || (tc.want != "" && !strings.Contains(got[0].Skip, tc.want)) {
				t.Fatalf("ancestor hold: %+v", got)
			}
		})
	}
}
