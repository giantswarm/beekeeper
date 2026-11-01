package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func TestIssueParkWaitsForCloseAndBoundsReads(t *testing.T) {
	w := parkingWatch(t, nil)
	reads := 0
	closed := false
	refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
		reads++
		s := github.Open
		if closed {
			s = github.Closed
		}
		return map[github.PR]github.RefState{refs[0]: {Kind: github.RefIssue, State: s}}, nil
	}
	if err := park(t, w, "--on", refOne, "wait for issue"); err != nil {
		t.Fatal(err)
	}
	if p := agentOf(t, w).Park; p.RefKind != github.RefIssue {
		t.Fatalf("park %+v", p)
	}
	st, _ := w.store.Read()
	if out := w.settledParks(t.Context(), st); len(out) != 0 {
		t.Fatal(out)
	}
	initial := reads
	for range 3 {
		st, _ = w.store.Read()
		if out := w.settledParks(t.Context(), st); len(out) != 0 {
			t.Fatal(out)
		}
	}
	if reads != initial {
		t.Fatalf("issue read every pass: %d -> %d", initial, reads)
	}
	closed = true
	w.now = w.now.Add(5 * time.Minute)
	st, _ = w.store.Read()
	if out := w.settledParks(t.Context(), st); len(out) != 1 || !strings.Contains(out[0].answer, "closed") {
		t.Fatalf("close: %+v", out)
	}
}

func TestParkRefusesUnknownReference(t *testing.T) {
	w := parkingWatch(t, nil)
	if err := park(t, w, "--on", refOne, "unknown"); err == nil || !strings.Contains(err.Error(), "not a readable") {
		t.Fatalf("unknown: %v", err)
	}
	if agentOf(t, w).Park != nil {
		t.Fatal("unknown ref recorded a park")
	}
}

func TestLoopCachesLegacyIssueParksAcrossRestarts(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, r.a.now)
	reads := 0
	refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
		reads++
		return map[github.PR]github.RefState{refs[0]: {Kind: github.RefIssue, State: github.Open}}, nil
	}
	readLoopPull = func(context.Context, string) (superloop.Pull, error) { panic("issue read as PR") }
	for range 3 {
		r.pass(t)
	}
	if reads != 1 {
		t.Fatalf("resolved %d times", reads)
	}
}

func TestIssueParkSuggestsLinkedPullForServedIssue(t *testing.T) {
	w := parkingWatch(t, nil)
	refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
		return map[github.PR]github.RefState{refs[0]: {Kind: github.RefIssue, State: github.Open}}, nil
	}
	prev := readParkSuggestions
	t.Cleanup(func() { readParkSuggestions = prev })
	const pullURL = "https://github.com/o/r/pull/2"
	readParkSuggestions = func(context.Context, github.PR) (string, error) { return pullURL, nil }
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Records = append(st.Records, state.Record{Session: st.Agents[0].Party, Issue: refOne})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	line, err := w.park(state.Party{Name: agentOne}, refOne, "desk review")
	if err != nil || !strings.Contains(line, pullURL) {
		t.Fatalf("suggestion %q %v", line, err)
	}
	if agentOf(t, w).Park.On != refOne {
		t.Fatal("suggestion changed the wait")
	}
}

func TestLoopDoesNotReadAnUnresolvedParkAsPull(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, r.a.now)
	refStates = func(context.Context, []github.PR) (map[github.PR]github.RefState, error) { return nil, nil }
	readLoopPull = func(context.Context, string) (superloop.Pull, error) {
		t.Fatal("unknown ref read as PR")
		return superloop.Pull{}, nil
	}
	r.pass(t)
}

// stubIssuePark keeps harness identity tests independent of GitHub credentials
// while exercising the new reference resolution before their park is recorded.
func stubIssuePark(t *testing.T, on string) {
	t.Helper()
	want, ok := parseRef(on)
	if !ok {
		t.Fatalf("bad fixture reference %q", on)
	}
	before, suggestions := refStates, readParkSuggestions
	t.Cleanup(func() { refStates, readParkSuggestions = before, suggestions })
	refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
		if len(refs) != 1 || refs[0] != want {
			t.Fatalf("park resolution %v, want %v", refs, want)
		}
		return map[github.PR]github.RefState{want: {Kind: github.RefIssue, State: github.Open}}, nil
	}
	readParkSuggestions = func(context.Context, github.PR) (string, error) { return "", nil }
}

func TestIssueParkOffTheRosterBoundsReadsAndSettles(t *testing.T) {
	w := parkingWatch(t, nil)
	reads, closed := 0, false
	refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
		reads++
		s := github.Open
		if closed {
			s = github.Closed
		}
		return map[github.PR]github.RefState{refs[0]: {Kind: github.RefIssue, State: s}}, nil
	}
	if _, err := w.park(offSession, refOne, "wait for issue"); err != nil {
		t.Fatal(err)
	}
	st, _ := w.store.Read()
	if out := w.settledParks(t.Context(), st); len(out) != 0 {
		t.Fatal(out)
	}
	initial := reads
	for range 3 {
		st, _ = w.store.Read()
		if len(st.Parks) != 1 || st.Parks[0].RefChecked.IsZero() || st.Parks[0].RefKind != github.RefIssue {
			t.Fatalf("cache %+v", st.Parks)
		}
		if out := w.settledParks(t.Context(), st); len(out) != 0 {
			t.Fatal(out)
		}
	}
	if reads != initial {
		t.Fatalf("off-roster issue read every pass: %d -> %d", initial, reads)
	}
	closed = true
	w.now = w.now.Add(5 * time.Minute)
	st, _ = w.store.Read()
	if out := w.settledParks(t.Context(), st); len(out) != 1 || !out[0].party.Is(offSession) || !strings.Contains(out[0].answer, "closed") {
		t.Fatalf("close %+v", out)
	}
}
