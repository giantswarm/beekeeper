package board

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
)

func TestReadRetainsAncestorsOutsideTheOrdersStatuses(t *testing.T) {
	for _, tc := range []struct{ name, status, kind, extra, want string }{
		{"inbox epic", "Inbox", `{"name":"Epic 🎯"}`, "", NotScheduled},
		{"statusless epic", "", `{"name":"Epic 🎯"}`, "", NotScheduled},
		{"blocked parent", blocked, "null", `,"blockedBy":{"totalCount":1,"nodes":[{"number":3,"state":"OPEN","repository":{"nameWithOwner":"o/r"}}]}`, HeldWord},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := config.Board{Owner: "o", Project: 7, Team: "X", Order: []config.BoardStep{{Name: "working", Status: []string{"in progress"}}}}
			c := Client{Board: b, GH: func(ctx context.Context, args ...string) ([]byte, error) {
				query := strings.Join(args, " ")
				if strings.Contains(query, "fields(first:50)") {
					return []byte(strings.Replace(metaJSON, `{"id":"s1"`, `{"id":"inbox","name":"Inbox"},{"id":"blocked","name":"`+blocked+`"},{"id":"s1"`, 1)), nil
				}
				if !strings.Contains(query, boardItemsQuery) {
					t.Fatalf("unexpected query: %s", query)
				}
				q := ""
				for _, arg := range args {
					if strings.HasPrefix(arg, "q=") {
						q = strings.TrimPrefix(arg, "q=")
					}
				}
				if !strings.Contains(q, `team:"X"`) {
					t.Errorf("team selection missing: %q", q)
				}
				nodes := []string{node(inProgress, "null", issue(2, `,"parent":{"number":1,"repository":{"nameWithOwner":"o/r"}}`)), node(inProgress, "null", issue(3, `,"parent":{"number":1,"repository":{"nameWithOwner":"o/r"}}`))}
				// Honor the actual status filter so an omitted ancestor cannot pass unnoticed.
				if !strings.Contains(q, "status:") || (tc.status != "" && strings.Contains(q, fmt.Sprintf("%q", tc.status))) {
					parent := node(tc.status, tc.kind, issue(1, tc.extra))
					if tc.status == "" {
						parent = strings.Replace(parent, `"status":{"name":""}`, `"status":null`, 1)
					}
					nodes = append([]string{parent}, nodes...)
				}
				return []byte(`{"data":{"repositoryOwner":{"projectV2":{"items":{"nodes":[` + strings.Join(nodes, ",") + `]}}}}}`), nil
			}}
			snap, err := c.Read(context.Background(), now)
			if err != nil {
				t.Fatal(err)
			}
			got := Rank(snap, b, now)
			if len(got) != 2 {
				t.Fatalf("order offered %d candidates, want 2: %+v", len(got), got)
			}
			if got[0].Ref != "o/r#2" || !strings.Contains(got[0].Skip, tc.want) {
				t.Errorf("child: %+v, want skip containing %q", got[0], tc.want)
			}
			if tc.want == HeldWord && got[1].Skip != "" {
				t.Errorf("blocker itself held: %+v", got[1])
			}
		})
	}
}
