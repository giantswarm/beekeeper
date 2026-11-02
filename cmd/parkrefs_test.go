package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
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

var errParkLookup = errors.New("GitHub lookup unavailable")

func TestParkRecordsFailedLookupThenClassifies(t *testing.T) {
	for _, kind := range []string{github.RefIssue, github.RefPullRequest} {
		t.Run(kind, func(t *testing.T) {
			w := parkingWatch(t, nil)
			refStates = func(context.Context, []github.PR) (map[github.PR]github.RefState, error) { return nil, errParkLookup }
			if err := park(t, w, "--on", refOne, "waiting for GitHub"); err != nil {
				t.Fatal(err)
			}
			if p := agentOf(t, w).Park; p == nil || p.On != refOne || p.RefKind != "" {
				t.Fatalf("unresolved park %+v", p)
			}
			refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
				return map[github.PR]github.RefState{refs[0]: {Kind: kind, State: github.Open}}, nil
			}
			st, _ := w.store.Read()
			if out := w.settledParks(t.Context(), st); len(out) != 0 {
				t.Fatal(out)
			}
			if p := agentOf(t, w).Park; p.RefKind != kind {
				t.Fatalf("classification %+v", p)
			}
			st, _ = w.store.Read()
			if got := w.parkIsPull(t.Context(), st, refOne); got != (kind == github.RefPullRequest) {
				t.Fatalf("PR classification %v for %s", got, kind)
			}
		})
	}
}

func TestManagedParkRecordsFailedLookupThenClassifies(t *testing.T) {
	a, r := executionTestApp(t)
	before := refStates
	t.Cleanup(func() { refStates = before })
	refStates = func(context.Context, []github.PR) (map[github.PR]github.RefState, error) { return nil, errParkLookup }
	kind, err := resolveParkRef(t.Context(), refOne)
	if err != nil || kind != "" {
		t.Fatalf("managed resolution %q %v", kind, err)
	}
	request := execution.ControlRequest{Ref: r.Ref, ID: "park-unresolved", Sequence: 1, Operation: execution.ControlPark, Data: mustExecutionJSON(t, executionPark{On: refOne, Waits: "waiting for GitHub", RefKind: kind, ReviewHead: loopHead})}
	var result execution.ControlResult
	if err := coordinatorTestCall(t, a, r, "control", request, &result, false); err != nil || result.Error != "" {
		t.Fatalf("managed park: %v %s", err, result.Error)
	}
	st, _ := a.store.Read()
	// The host's own review head is no proof: the coordinator reads the
	// pull request itself, and o/r is not the execution's repository.
	if p := st.Agents[0].Park; p == nil || p.On != refOne || p.RefKind != "" || p.ReviewHead != "" {
		t.Fatalf("unresolved managed park %+v", p)
	}
	refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
		return map[github.PR]github.RefState{refs[0]: {Kind: github.RefPullRequest, State: github.Open}}, nil
	}
	if !a.parkIsPull(t.Context(), st, refOne) {
		t.Fatal("managed park was not classified as PR")
	}
	st, _ = a.store.Read()
	if st.Agents[0].Park.RefKind != github.RefPullRequest {
		t.Fatal("managed classification not persisted")
	}
}

func TestLoopBacksOffUnansweredLegacyParksAcrossRestarts(t *testing.T) {
	for _, readErr := range []error{nil, errParkLookup} {
		name := "unanswered reference"
		if readErr != nil {
			name = "unavailable"
		}
		t.Run(name, func(t *testing.T) {
			r := newLoopRig(t)
			r.park(t, loopPR, r.a.now)
			reads := 0
			refStates = func(context.Context, []github.PR) (map[github.PR]github.RefState, error) {
				reads++
				return nil, readErr
			}
			readLoopPull = func(context.Context, string) (superloop.Pull, error) {
				t.Fatal("unresolved ref read as PR")
				return superloop.Pull{}, nil
			}
			for range 3 {
				r.pass(t)
			}
			if reads != 1 {
				t.Fatalf("unresolved reference read %d times", reads)
			}
			st, _ := r.a.store.Read()
			if st.Agents[0].Park.RefChecked.IsZero() {
				t.Fatal("attempt time not persisted")
			}
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents[0].Park.RefChecked = st.Agents[0].Park.RefChecked.Add(-parkRefRetry)
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
				reads++
				return map[github.PR]github.RefState{refs[0]: {Kind: github.RefIssue, State: github.Open}}, nil
			}
			r.pass(t)
			st, _ = r.a.store.Read()
			if reads != 2 || st.Agents[0].Park.RefKind != github.RefIssue {
				t.Fatalf("retry %d, park %+v", reads, st.Agents[0].Park)
			}
		})
	}
}

func TestWatcherBacksOffFailedLegacyParkClassification(t *testing.T) {
	for _, offRoster := range []bool{false, true} {
		name := "roster"
		if offRoster {
			name = "off-roster"
		}
		t.Run(name, func(t *testing.T) {
			w := parkingWatch(t, nil)
			if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
				if offRoster {
					st.Parks = []state.Park{{By: offSession, At: w.now, On: refOne}}
				} else {
					st.Agents[0].Park = &state.Park{At: w.now, On: refOne}
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			reads := 0
			refStates = func(context.Context, []github.PR) (map[github.PR]github.RefState, error) {
				reads++
				return nil, errParkLookup
			}
			for range 3 {
				st, _ := w.store.Read()
				if out := w.settledParks(t.Context(), st); len(out) != 0 {
					t.Fatal(out)
				}
			}
			if reads != 1 {
				t.Fatalf("failed classification read %d times", reads)
			}
			w.now = w.now.Add(parkRefRetry)
			refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
				reads++
				return map[github.PR]github.RefState{refs[0]: {Kind: github.RefIssue, State: github.Open}}, nil
			}
			st, _ := w.store.Read()
			w.settledParks(t.Context(), st)
			st, _ = w.store.Read()
			typed := false
			for _, ag := range parkedAgents(st) {
				if ag.Park != nil && ag.Park.RefKind == github.RefIssue {
					typed = true
				}
			}
			if reads != 2 || !typed {
				t.Fatalf("retry %d, typed %v", reads, typed)
			}
		})
	}
}

func TestParkSuggestionMatchesWholeIssueReferences(t *testing.T) {
	w := parkingWatch(t, nil)
	before := readParkSuggestions
	t.Cleanup(func() { readParkSuggestions = before })
	calls := 0
	readParkSuggestions = func(context.Context, github.PR) (string, error) { calls++; return "linked PR", nil }
	for _, tc := range []struct {
		task string
		want bool
	}{
		{"Work o/r#12", false},
		{"Work https://github.com/o/r/issues/12", false},
		{"Work o/r#1.", true},
		{"Work https://github.com/o/r/issues/1", true},
	} {
		if err := w.store.Update(func(st *state.State) ([]state.Event, error) { st.Agents[0].Task = tc.task; return nil, nil }); err != nil {
			t.Fatal(err)
		}
		calls = 0
		got := w.parkSuggestion(state.Party{Name: agentOne}, refOne)
		if (got != "") != tc.want || (calls == 1) != tc.want {
			t.Errorf("%q: suggestion %q, calls %d", tc.task, got, calls)
		}
	}
}
