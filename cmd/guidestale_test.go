//go:build unix

package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/project"
)

const guideBinary = "/home/u/.go/bin/beekeeper"

func testGuideSelf(replaced *bool) *guideSelf {
	return &guideSelf{
		pid:       4242,
		args:      strings.Fields(guideBinary + " guide watch"),
		replaced:  func() (string, bool) { return guideBinary, *replaced },
		versionOf: func(context.Context, string) string { return "v9.9.9" },
	}
}

// A guide watch whose binary self-update replaced says so once, with the
// command that re-arms it, and keeps saying its feed.
func TestGuideWatchSaysItsReplacedBinaryOnce(t *testing.T) {
	replaced := false
	g := testGuideSelf(&replaced)
	if got := g.lines(t.Context(), []string{"GUIDE DECISION: #1"}, nil); len(got) != 1 || got[0] != "GUIDE DECISION: #1" {
		t.Fatalf("a current binary's poll: %q", got)
	}
	replaced = true
	got := g.lines(t.Context(), []string{"GUIDE DECISION: #2"}, nil)
	want := "GUIDE WATCH STALE: this watch (pid 4242) runs " + project.Version() + "; self-update installed v9.9.9 at " + guideBinary +
		", a re-arm picks it up: stop it, then run `beekeeper guide watch`"
	if len(got) != 2 || got[0] != want || got[1] != "GUIDE DECISION: #2" {
		t.Fatalf("the replaced binary's poll:\n%q\nwant %q and the feed", got, want)
	}
	if got := g.lines(t.Context(), nil, nil); got != nil {
		t.Fatalf("the next poll repeats the stale binary: %q", got)
	}
}

// A guide watch whose save a newer release refuses says so, with the
// command that re-arms it and the lines the refused save would have
// recorded, and says it again only when those lines change.
func TestGuideWatchReportsARefusedSave(t *testing.T) {
	a, _ := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	newer, err := state.OpenVersion(a.store.Dir(), newerRelease)
	if err != nil {
		t.Fatal(err)
	}
	owner := state.Party{Session: "sO", Name: agentC.Name}
	if err := newer.Update(func(st *state.State) ([]state.Event, error) {
		st.Notes = []state.Note{{ID: 1, For: personTimo, Text: uiNoteText, By: owner}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if a.store, err = state.OpenVersion(a.store.Dir(), olderRelease); err != nil {
		t.Fatal(err)
	}
	a.now = time.Now()

	feed, err := a.guideFeed(t.Context())
	var stale *state.StaleWriterError
	if !errors.As(err, &stale) {
		t.Fatalf("the older release's feed saved: %v", err)
	}
	replaced := false
	g := testGuideSelf(&replaced)
	got := g.lines(t.Context(), feed, err)
	if len(got) != 2 || !strings.HasPrefix(got[0], "GUIDE SAVE REFUSED: pid ") ||
		!strings.Contains(got[0], "runs beekeeper v0.1.0, older than the v0.2.0 that wrote the state: its save is refused") ||
		!strings.HasSuffix(got[0], "; re-arm this watch: stop it, then run `beekeeper guide watch`") ||
		!strings.HasPrefix(got[1], "GUIDE DECISION: #1 for Timo") {
		t.Fatalf("the refused save's poll: %q", got)
	}

	feed, err = a.guideFeed(t.Context())
	if got := g.lines(t.Context(), feed, err); got != nil {
		t.Fatalf("the same refused save is said again: %q", got)
	}

	if err := newer.Update(func(st *state.State) ([]state.Event, error) {
		st.Notes = append(st.Notes, state.Note{ID: 2, For: personTimo, Text: "pick a name", By: owner})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	feed, err = a.guideFeed(t.Context())
	got = g.lines(t.Context(), feed, err)
	if len(got) != 3 || !strings.HasPrefix(got[0], "GUIDE SAVE REFUSED: ") || !strings.HasPrefix(got[2], "GUIDE DECISION: #2 for Timo") {
		t.Fatalf("a refused save of a new decision: %q", got)
	}

	st, err := newer.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Writer == nil || st.Writer.Version != newerRelease || len(st.GuideRole().Fed) != 0 {
		t.Errorf("writer %+v, fed %q: the older release's feed saved", st.Writer, st.GuideRole().Fed)
	}
}
