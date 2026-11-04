package board

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
)

func TestMoveRefusesInvalidGateBeforeAddingAnItem(t *testing.T) {
	f := &fakeGH{t: t, item: issue7OffBoard, added: `{"data":{"addProjectV2ItemById":{"item":{"id":"I70","project":{"id":"P"}}}}}`}
	c := &Client{GH: f.run, Board: config.Board{Owner: "o", Project: 7, Epics: config.EpicGate{Kind: []string{"missing kind"}}}}
	_, err := c.Move(t.Context(), engineIssue7, "backlog")
	var refusal *Refusal
	if !errors.As(err, &refusal) || len(f.mutations) != 0 {
		t.Fatalf("invalid gate: error=%v, mutations=%v", err, f.mutations)
	}
}

func TestMoveHeldRefusesInvalidMoveBeforeRecordingABlocker(t *testing.T) {
	for _, badTarget := range []bool{true, false} {
		t.Run(map[bool]string{true: "pull request target", false: "unknown team"}[badTarget], func(t *testing.T) {
			deps := newFakeDeps(t)
			f := &fakeGH{t: t, item: onBoard, byIssue: map[string]string{"15": `{"data":{"repository":{"issue":null}}}`}}
			c := &Client{Board: config.Board{Owner: "o", Project: 7, Team: "X"}, GH: func(ctx context.Context, args ...string) ([]byte, error) {
				switch q := args[3]; {
				case strings.Contains(q, "fields(first:50)"):
					return []byte(blockedMeta), nil
				case strings.Contains(q, "issueOrPullRequest") || strings.Contains(q, "BlockedBy("):
					return deps.run(ctx, args...)
				}
				return f.run(ctx, args...)
			}}
			ref := issue5
			if badTarget {
				ref = "o/r#15"
			} else {
				c.Board.Team = "unknown team"
			}
			_, _, err := c.MoveHeld(t.Context(), ref, "blocked", issue9)
			var refusal *Refusal
			if !errors.As(err, &refusal) || len(deps.mutations)+len(f.mutations) != 0 {
				t.Fatalf("invalid blocked move: error=%v, dependency mutations=%v, board mutations=%v", err, deps.mutations, f.mutations)
			}
		})
	}
}
