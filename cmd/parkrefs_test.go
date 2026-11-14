package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
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
	st, _ := watchApp(w.Host).store.Read()
	if out := w.SettledParks(t.Context(), st); len(out) != 0 {
		t.Fatal(out)
	}
	initial := reads
	for range 3 {
		st, _ = watchApp(w.Host).store.Read()
		if out := w.SettledParks(t.Context(), st); len(out) != 0 {
			t.Fatal(out)
		}
	}
	if reads != initial {
		t.Fatalf("issue read every pass: %d -> %d", initial, reads)
	}
	closed = true
	watchApp(w.Host).now = watchApp(w.Host).now.Add(5 * time.Minute)
	st, _ = watchApp(w.Host).store.Read()
	if out := w.SettledParks(t.Context(), st); len(out) != 1 || !strings.Contains(out[0].Answer, overrideClosed) {
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
	loop.ReadLoopPull = func(context.Context, string) (superloop.Pull, error) { panic("issue read as PR") }
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
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Records = append(st.Records, state.Record{Session: st.Agents[0].Party, Issue: refOne})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	line, err := watchApp(w.Host).park(state.Party{Name: agentOne}, refOne, "desk review")
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
	loop.ReadLoopPull = func(context.Context, string) (superloop.Pull, error) {
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
	if _, err := watchApp(w.Host).park(offSession, refOne, "wait for issue"); err != nil {
		t.Fatal(err)
	}
	st, _ := watchApp(w.Host).store.Read()
	if out := w.SettledParks(t.Context(), st); len(out) != 0 {
		t.Fatal(out)
	}
	initial := reads
	for range 3 {
		st, _ = watchApp(w.Host).store.Read()
		if len(st.Parks) != 1 || st.Parks[0].RefChecked.IsZero() || st.Parks[0].RefKind != github.RefIssue {
			t.Fatalf("cache %+v", st.Parks)
		}
		if out := w.SettledParks(t.Context(), st); len(out) != 0 {
			t.Fatal(out)
		}
	}
	if reads != initial {
		t.Fatalf("off-roster issue read every pass: %d -> %d", initial, reads)
	}
	closed = true
	watchApp(w.Host).now = watchApp(w.Host).now.Add(5 * time.Minute)
	st, _ = watchApp(w.Host).store.Read()
	if out := w.SettledParks(t.Context(), st); len(out) != 1 || !out[0].Party.Is(offSession) || !strings.Contains(out[0].Answer, overrideClosed) {
		t.Fatalf("close %+v", out)
	}
}

var errParkLookup = errors.New("GitHub lookup unavailable")

func TestParkRefusesFailedReadsThenRetries(t *testing.T) {
	for _, kind := range []string{github.RefIssue, github.RefPullRequest} {
		t.Run(kind, func(t *testing.T) {
			w := parkingWatch(t, nil)
			refStates = func(context.Context, []github.PR) (map[github.PR]github.RefState, error) { return nil, errParkLookup }
			if err := park(t, w, "--on", refOne, "waiting for GitHub"); err == nil {
				t.Fatal("failed reads accepted")
			}
			if p := agentOf(t, w).Park; p != nil {
				t.Fatalf("unresolved park %+v", p)
			}
			refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
				return map[github.PR]github.RefState{refs[0]: {Kind: kind, State: github.Open}}, nil
			}
			if err := park(t, w, "--on", refOne, "waiting for GitHub"); err != nil {
				t.Fatal(err)
			}
			st, _ := watchApp(w.Host).store.Read()
			if out := w.SettledParks(t.Context(), st); len(out) != 0 {
				t.Fatal(out)
			}
			if p := agentOf(t, w).Park; p.RefKind != kind {
				t.Fatalf("classification %+v", p)
			}
			st, _ = watchApp(w.Host).store.Read()
			if got := watchApp(w.Host).parkIsPull(t.Context(), st, refOne); got != (kind == github.RefPullRequest) {
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
	beforeREST := readParkRefREST
	t.Cleanup(func() { readParkRefREST = beforeREST })
	readParkRefREST = func(context.Context, github.PR) (github.RefState, error) { return github.RefState{}, errParkLookup }
	kind, err := resolveParkRef(t.Context(), refOne)
	if err == nil || !strings.Contains(err.Error(), errParkLookup.Error()) {
		t.Fatalf("managed resolution %q %v", kind, err)
	}
	postMerge := "remaining proof"
	request := execution.ControlRequest{Ref: r.Ref, ID: "park-unresolved", Sequence: 1, Operation: execution.ControlPark, Data: mustExecutionJSON(t, executionPark{On: refOne, Waits: "waiting for GitHub", RefKind: kind, ReviewHead: loopHead, PostMerge: &postMerge})}
	var result execution.ControlResult
	if err := coordinatorTestCall(t, a, r, "control", request, &result, false); err != nil || result.Error != "" {
		t.Fatalf("managed park: %v %s", err, result.Error)
	}
	st, _ := a.store.Read()
	// The host's own review head is no proof: the coordinator reads the
	// pull request itself, and o/r is not the execution's repository.
	if p := st.Agents[0].Park; p == nil || p.On != refOne || p.RefKind != "" || p.ReviewHead != "" || p.PostMerge != nil {
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
			loop.ReadLoopPull = func(context.Context, string) (superloop.Pull, error) {
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
			if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
				if offRoster {
					st.Parks = []state.Park{{By: offSession, At: watchApp(w.Host).now, On: refOne}}
				} else {
					st.Agents[0].Park = &state.Park{At: watchApp(w.Host).now, On: refOne}
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
				st, _ := watchApp(w.Host).store.Read()
				if out := w.SettledParks(t.Context(), st); len(out) != 0 {
					t.Fatal(out)
				}
			}
			if reads != 1 {
				t.Fatalf("failed classification read %d times", reads)
			}
			watchApp(w.Host).now = watchApp(w.Host).now.Add(parkRefRetry)
			refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
				reads++
				return map[github.PR]github.RefState{refs[0]: {Kind: github.RefIssue, State: github.Open}}, nil
			}
			st, _ := watchApp(w.Host).store.Read()
			w.SettledParks(t.Context(), st)
			st, _ = watchApp(w.Host).store.Read()
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
		if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) { st.Agents[0].Task = tc.task; return nil, nil }); err != nil {
			t.Fatal(err)
		}
		calls = 0
		got := watchApp(w.Host).parkSuggestion(state.Party{Name: agentOne}, refOne)
		if (got != "") != tc.want || (calls == 1) != tc.want {
			t.Errorf("%q: suggestion %q, calls %d", tc.task, got, calls)
		}
	}
}

func TestParkRefusesSettledReference(t *testing.T) {
	for _, target := range []struct{ kind, status string }{
		{github.RefIssue, github.Closed},
		{github.RefPullRequest, github.Closed},
		{github.RefPullRequest, github.Merged},
	} {
		t.Run(target.kind+"/"+target.status, func(t *testing.T) {
			for _, offRoster := range []bool{false, true} {
				w := parkingWatch(t, nil)
				refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
					return map[github.PR]github.RefState{refs[0]: {Kind: target.kind, State: target.status}}, nil
				}
				var err error
				if offRoster {
					_, err = watchApp(w.Host).park(offSession, refOne, "wait for target")
				} else {
					err = park(t, w, "--on", refOne, "wait for target")
				}
				if err == nil || !strings.Contains(err.Error(), "already "+strings.ToLower(target.status)) {
					t.Fatalf("settled target: %v", err)
				}
				st, err := watchApp(w.Host).store.Read()
				if err != nil {
					t.Fatal(err)
				}
				if st.Agents[0].Park != nil || len(st.Parks) != 0 || len(st.ReviewParks) != 0 {
					t.Fatal("settled target recorded a park")
				}
				if settled := w.SettledParks(t.Context(), st); len(settled) != 0 {
					t.Fatalf("refused park can resume: %+v", settled)
				}
			}
		})
	}
}

func TestParkReferenceRESTFallback(t *testing.T) {
	originalHead := parkReviewHead
	t.Cleanup(func() { parkReviewHead = originalHead })
	parkReviewHead = func(string) (string, string) { return loopHead, "" }
	originalREST, originalGH := readParkRefREST, github.RunGH
	t.Cleanup(func() { github.RunGH = originalGH })
	for _, on := range []string{refOne, "https://github.com/o/r/pull/1"} {
		t.Run(on, func(t *testing.T) {
			w := parkingWatch(t, nil)
			readParkRefREST = originalREST
			github.RunGH = func(ctx context.Context, args ...string) ([]byte, error) {
				if github.Repo(ctx) != repo || strings.Join(args, " ") != "api repos/o/r/issues/1" {
					t.Fatalf("identity %q args %v", github.Repo(ctx), args)
				}
				return []byte(`{"state":"open","pull_request":{"merged_at":null}}`), nil
			}
			if err := park(t, w, "--on", on, "desk review"); err != nil {
				t.Fatal(err)
			}
			if p := agentOf(t, w).Park; p == nil || p.On != refOne || p.RefKind != github.RefPullRequest || p.ReviewHead != loopHead {
				t.Fatalf("park %+v", p)
			}
		})
	}
}

func TestParkReferenceRESTFailures(t *testing.T) {
	originalREST, originalGH := readParkRefREST, github.RunGH
	t.Cleanup(func() { github.RunGH = originalGH })
	for _, tc := range []struct {
		name, body string
		err        error
	}{
		{name: "access", err: errors.New("HTTP 403: access denied")},
		{name: "rate limit", err: errors.New("HTTP 429: rate limit exceeded")},
		{name: "malformed", body: "{"},
		{name: "missing", body: `{}`},
		{name: "closed target", body: `{"state":"closed","pull_request":{"merged_at":null}}`},
		{name: "merged target", body: `{"state":"closed","pull_request":{"merged_at":"2026-01-01T00:00:00Z"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := parkingWatch(t, nil)
			readParkRefREST = originalREST
			refStates = func(context.Context, []github.PR) (map[github.PR]github.RefState, error) { return nil, errParkLookup }
			github.RunGH = func(context.Context, ...string) ([]byte, error) { return []byte(tc.body), tc.err }
			err := park(t, w, "--on", refOne, "desk review")
			if err == nil {
				t.Fatal("failed or settled read accepted")
			}
			if tc.name != "closed target" && tc.name != "merged target" && (!strings.Contains(err.Error(), "repos/o/r/issues/1") || !strings.Contains(err.Error(), errParkLookup.Error())) {
				t.Fatal(err)
			}
			if tc.err != nil && !strings.Contains(err.Error(), tc.err.Error()) {
				t.Fatal(err)
			}
			if agentOf(t, w).Park != nil {
				t.Fatal("failed read recorded a park")
			}
		})
	}
}
