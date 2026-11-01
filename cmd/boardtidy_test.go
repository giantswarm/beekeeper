package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// tidyAnswers answers board tidy's requests: one closed item In Progress,
// closed three weeks ago; writes counts the mutations.
func tidyAnswers(writes *int) func(context.Context, ...string) ([]byte, error) {
	return func(_ context.Context, args ...string) ([]byte, error) {
		q := args[3]
		switch {
		case strings.Contains(q, "mutation"):
			*writes++
			return []byte(`{"data":{"x":{"item":{"id":"I"}}}}`), nil
		case strings.Contains(q, "fields(first:50)"):
			return []byte(`{"data":{"repositoryOwner":{"projectV2":{"id":"P","fields":{"nodes":[{"id":"FS","name":"Status","options":[{"id":"s3","name":"In Progress"},{"id":"s9","name":"Done"}]}]}}}}}`), nil
		}
		return []byte(`{"data":{"repositoryOwner":{"projectV2":{"items":{"pageInfo":{"hasNextPage":false},"nodes":[
{"id":"PVTI_1","isArchived":false,"status":{"name":"In Progress"},"content":{"number":1,"url":"https://github.com/o/r/issues/1","state":"CLOSED","closedAt":"2026-10-01T00:00:00Z","repository":{"nameWithOwner":"o/r"}}}]}}}}}`), nil
	}
}

func TestBoardTidySaysWhatItDidWithTheRestore(t *testing.T) {
	for _, dry := range []bool{false, true} {
		a, out := noteApp(t)
		a.now = time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
		a.cfg.Board = config.Board{Owner: "o", Project: 7, ArchiveAfter: config.Duration{Duration: config.DefaultBoardArchiveAfter}}
		var writes int
		was := boardGH
		boardGH = tidyAnswers(&writes)
		t.Cleanup(func() { boardGH = was })
		c := a.boardTidyCmd()
		args := []string{}
		if dry {
			args = append(args, "--dry-run")
		}
		c.SetArgs(args)
		c.SetOut(out)
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
		line := "https://github.com/o/r/issues/1 closed 2026-10-01: In Progress → Done, archived (restore: gh project item-archive 7 --owner o --id PVTI_1 --undo)"
		if dry {
			line = "would: " + line
		}
		if strings.TrimSpace(out.String()) != line {
			t.Errorf("dry %t: out %q, want %q", dry, out, line)
		}
		evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "board.tidy" })
		if err != nil {
			t.Fatal(err)
		}
		if want := map[bool]int{false: 2, true: 0}[dry]; writes != want || len(evs) != want/2 {
			t.Errorf("dry %t: %d writes, %d events", dry, writes, len(evs))
		}
	}
}
