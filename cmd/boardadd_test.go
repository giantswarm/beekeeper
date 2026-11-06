package cmd

import (
	"context"
	"strings"
	"testing"
)

// boardAddRef is the issue the board add and show commands act on.
const boardAddRef = "o/r#5"

func TestBoardAddShowCommands(t *testing.T) {
	for _, name := range []string{loopAdd, noteShowName} {
		t.Run(name, func(t *testing.T) {
			a, _ := stubApp(t)
			a.cfg.Board.Owner, a.cfg.Board.Project = "o", 7
			var out strings.Builder
			a.out = &out
			was := boardGH
			t.Cleanup(func() { boardGH = was })
			writes := 0
			boardGH = func(_ context.Context, args ...string) ([]byte, error) {
				q := args[3]
				if strings.Contains(q, "items(") {
					t.Fatalf("whole-board read: %s", q)
				}
				switch {
				case strings.Contains(q, "fields(first:50)"):
					return []byte(`{"data":{"repositoryOwner":{"projectV2":{"id":"P","fields":{"nodes":[{"id":"S","name":"Status","options":[{"id":"b","name":"Backlog"}]},{"id":"K","name":"Kind","options":[{"id":"k","name":"Bug"}]}]}}}}}`), nil
				case strings.Contains(q, "projectItems"):
					return []byte(`{"data":{"repository":{"issue":{"id":"ISSUE","projectItems":{"nodes":[{"id":"I","project":{"id":"P"},"status":{"name":"Backlog"},"kind":{"name":"Bug"}}]}}}}}`), nil
				case strings.Contains(q, "updateProjectV2ItemFieldValue"):
					writes++
					return []byte(`{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"I"}}}}`), nil
				}
				t.Fatalf("unexpected query: %s", q)
				return nil, nil
			}
			c := a.boardCmd()
			args := []string{name, boardAddRef}
			if name == loopAdd {
				args = append(args, "--kind", "bug")
			}
			c.SetArgs(args)
			if err := c.Execute(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "Status Backlog; Kind Bug") {
				t.Fatalf("output: %s", out.String())
			}
			if name == noteShowName && writes != 0 || name == loopAdd && writes != 2 {
				t.Fatalf("writes: %d", writes)
			}
		})
	}
}
