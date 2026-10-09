package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// onceWatch is a --once watch (it writes nothing) on a note linked to refOne
// and refTwo, with GitHub answering read.
func onceWatch(t *testing.T, read func(context.Context, []github.PR) (map[github.PR]github.RefState, error)) (*watcher, *strings.Builder) {
	t.Helper()
	w, _ := overtakingWatch(t, nil)
	w.chores = false
	refStates = read
	var out strings.Builder
	w.out = &out
	setNotes(t, w, state.Note{ID: 1, For: personTimo, Text: askedQ, By: state.Party{Name: supRun3}, Refs: []string{refOne, refTwo}})
	return w, &out
}

// openRefs answers every ref open.
func openRefs(_ context.Context, rs []github.PR) (map[github.PR]github.RefState, error) {
	m := map[github.PR]github.RefState{}
	for _, r := range rs {
		m[r] = github.RefState{State: github.Open}
	}
	return m, nil
}

func TestWatchOnceSaysAFailedNotesReadAndExitsNonZero(t *testing.T) {
	w, out := onceWatch(t, func(context.Context, []github.PR) (map[github.PR]github.RefState, error) {
		return nil, errors.New("gh: connection reset")
	})
	w.pending(context.Background(), nil)
	if said := out.String(); !strings.Contains(said, "NOTE REFS UNREADABLE: cannot read the notes' issues and pull requests: gh: connection reset") {
		t.Errorf("watch --once said %q, want the failed read on the NOTE path", said)
	}
	if err := w.onceErr(); err == nil {
		t.Error("watch --once with a failed read exits 0")
	}
}

// The machine is the fixture's: under the floor without room for a start,
// the watch reads its headroom and says nothing.
func TestWatchOnceWithoutChangeIsSilent(t *testing.T) {
	w, out := onceWatch(t, openRefs)
	reads := 0
	w.readHeadroom = func(context.Context, *swapReading) *headroom { reads++; return tight() }
	w.pending(context.Background(), nil)
	if said := out.String(); said != "" {
		t.Errorf("watch --once without a change said %q", said)
	}
	if reads != 1 {
		t.Errorf("headroom read %d times, want once from the fixture", reads)
	}
	if err := w.onceErr(); err != nil {
		t.Errorf("watch --once without a change: %v", err)
	}
}

func TestWatchOnceSaysWhatKeepsTheNotesUnread(t *testing.T) {
	w, out := onceWatch(t, func(ctx context.Context, rs []github.PR) (map[github.PR]github.RefState, error) {
		m, _ := openRefs(ctx, rs[:1])
		return m, nil
	})
	w.pending(context.Background(), nil)
	if said := out.String(); !strings.Contains(said, "NOTE REFS UNANSWERED (--once): "+refTwo+"; their notes stay open") {
		t.Errorf("watch --once said %q, want the unanswered ref", said)
	}
	if err := w.onceErr(); err != nil {
		t.Errorf("an unanswered ref is no failed read: %v", err)
	}

	w, out = onceWatch(t, func(context.Context, []github.PR) (map[github.PR]github.RefState, error) {
		t.Error("GitHub read under the budget's floor")
		return nil, nil
	})
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Budget = &state.Budget{Remaining: 0, Limit: 5000, Reset: relayNow.Add(time.Hour), At: relayNow}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	w.pending(context.Background(), nil)
	if said := out.String(); !strings.Contains(said, "NOTE REFS NOT READ (--once): the GitHub budget is under its floor until") {
		t.Errorf("watch --once said %q, want the skipped read", said)
	}
}
