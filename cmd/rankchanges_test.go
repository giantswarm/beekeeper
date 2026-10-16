package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A change of a ranked epic's Team that beekeeper did not write is a
// board.changed event and a line; the first read and beekeeper's own writes
// (recordRanked) are none, and an epic that left the list closed is no
// change, read on its own item.
func TestRankChangesLogsWhatBeekeeperDidNotWrite(t *testing.T) {
	var asked []string
	defer func(gh func(context.Context, ...string) ([]byte, error)) { boardGH = gh }(boardGH)
	boardGH = func(_ context.Context, args ...string) ([]byte, error) {
		q := args[3]
		asked = append(asked, q)
		switch {
		case strings.Contains(q, "fields(first:50)"):
			return []byte(`{"data":{"repositoryOwner":{"projectV2":{"id":"P","fields":{"nodes":[{"id":"FS","name":"Status","options":[]}]}}}}}`), nil
		case strings.Contains(q, "projectItems"):
			return []byte(`{"data":{"repository":{"issue":{"number":3,"url":"https://github.com/o/r/issues/3","state":"CLOSED","repository":{"nameWithOwner":"o/r"},"projectItems":{"nodes":[]}}}}}`), nil
		}
		t.Fatalf("unexpected request %v", args)
		return nil, nil
	}
	a, store := priorityApp(t, "")
	cl := a.boardClient()
	ctx := context.Background()
	list := board.Priorities{{Rank: 1, Ref: refOne, Team: "X 🐝"}, {Rank: 2, Ref: "o/r#3", Team: "X 🐝"}}
	if got := a.rankChanges(ctx, cl, list); got != nil {
		t.Fatalf("first read: %v", got)
	}
	moved := board.Priorities{{Rank: 1, Ref: refOne, Team: "Y", TeamBy: "automation"}}
	got := a.rankChanges(ctx, cl, moved)
	want := []string{"changed outside beekeeper: " + refOne + ": Team X 🐝 → Y by automation, not by beekeeper"}
	if !slices.Equal(got, want) {
		t.Errorf("changes = %q, want %q", got, want)
	}
	if len(asked) != 2 {
		t.Errorf("GitHub asked %d times, want the closed epic's own item read once (and the board's fields)", len(asked))
	}
	evs, err := store.Events(0, func(e state.Event) bool { return e.Verb == "board.changed" })
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Detail, "Team X 🐝 → Y by automation") {
		t.Errorf("events = %+v, %v", evs, err)
	}
	if got := a.rankChanges(ctx, cl, moved); got != nil {
		t.Errorf("an unchanged read: %v", got)
	}
	a.recordRanked(list)
	if got := a.rankChanges(ctx, cl, list); got != nil {
		t.Errorf("after beekeeper's own write: %v", got)
	}
}
