package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	retireFixturePark = "giantswarm/beekeeper#2284"
	retireFixtureStop = "fixture-stop"
)

type retireTransport struct {
	*testExecutionTransport
	requests []execution.RetireRequest
	fail     error
}

func (f *retireTransport) Retire(_ context.Context, ref execution.Ref, q execution.RetireRequest) (execution.Retirement, error) {
	f.requests = append(f.requests, q)
	if f.fail != nil {
		return execution.Retirement{}, f.fail
	}
	return execution.Retirement{Ref: ref, ID: q.ID, Trigger: q.Trigger, Stage: execution.RetireRetired, RetainUntil: time.Now().Add(time.Hour)}, nil
}

// retireFixture is an ended execution whose log reached the transcript store,
// shaped by change, and the watcher and transport that retire it.
func retireFixture(t *testing.T, change func(*state.State, *state.ExecutionRecord)) (*app, *watcher, *retireTransport, execution.Ref) {
	t.Helper()
	a, r := executionTestApp(t)
	fake := &retireTransport{testExecutionTransport: &testExecutionTransport{}}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		rec := st.Starts[0].Execution
		rec.Observation.ProcessState = execution.Exited
		rec.TranscriptCollected = true
		if change != nil {
			change(st, rec)
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	return a, &watcher{app: a}, fake, r.Ref
}

func retirementOf(t *testing.T, a *app, ref execution.Ref) *state.ExecutionRetirement {
	t.Helper()
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := executionRecord(st, ref)
	return rec.Retirement
}

func tick(t *testing.T, a *app, w *watcher) {
	t.Helper()
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	w.retireEndedExecutions(context.Background(), st)
}

func TestExecutionRetirementTriggers(t *testing.T) {
	park := &state.Park{On: retireFixturePark}
	for _, tc := range []struct {
		name   string
		change func(*state.State, *state.ExecutionRecord)
		want   string
	}{
		{name: "running worker", change: func(_ *state.State, r *state.ExecutionRecord) { r.Observation.ProcessState = execution.Running }},
		{name: "crash", want: execution.RetireTriggerCrash},
		{name: "crashed fixer inherited a review record", want: execution.RetireTriggerCrash, change: func(st *state.State, r *state.ExecutionRecord) {
			ref := r.Handle.Ref
			r.Observation.ProcessState = execution.Failed
			st.ReviewParks = []state.ReviewPark{{On: retireFixturePark, Execution: &ref}}
		}},
		{name: "never accepted its task", want: execution.RetireTriggerFailedStart, change: func(_ *state.State, r *state.ExecutionRecord) { r.Observation.Ready = false }},
		{name: "released reservation", want: execution.RetireTriggerFailedStart, change: func(st *state.State, r *state.ExecutionRecord) {
			st.Agents = nil
			r.ReleasedAt = time.Now()
		}},
		{name: "final report", want: execution.RetireTriggerReport, change: func(st *state.State, r *state.ExecutionRecord) {
			st.Agents[0].Done = true
			r.Completed = true
		}},
		{name: "reported and left the roster", want: execution.RetireTriggerReport, change: func(st *state.State, r *state.ExecutionRecord) {
			st.Agents = nil
			r.Completed = true
		}},
		{name: "stop requested", want: execution.RetireTriggerStop, change: func(_ *state.State, r *state.ExecutionRecord) {
			r.StopRequest = &execution.StopRequest{ID: retireFixtureStop, Reason: "operator stop"}
		}},
		{name: "parked with an ended unit", change: func(st *state.State, _ *state.ExecutionRecord) { st.Agents[0].Park = park }},
		{name: "review park completes the round, not the task", change: func(st *state.State, r *state.ExecutionRecord) {
			st.Agents[0].Park = park
			r.Completed = true
		}},
		{name: "round completed, task open", change: func(_ *state.State, r *state.ExecutionRecord) { r.Completed = true }},
		{name: "open desk review after the agent left", change: func(st *state.State, r *state.ExecutionRecord) {
			ref := r.Handle.Ref
			st.Agents = nil
			r.Completed = true
			st.ReviewParks = []state.ReviewPark{{On: retireFixturePark, Execution: &ref}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			retireFixture(t, func(st *state.State, r *state.ExecutionRecord) {
				if tc.change != nil {
					tc.change(st, r)
				}
				got = executionRetirementTrigger(st, r)
			})
			if got != tc.want {
				t.Fatalf("trigger = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExecutionWatchRetiresOnceAndKeepsParkedWorkers(t *testing.T) {
	a, w, fake, ref := retireFixture(t, func(st *state.State, r *state.ExecutionRecord) {
		st.Agents[0].Park = &state.Park{On: retireFixturePark}
	})
	tick(t, a, w)
	if len(fake.requests) != 0 || retirementOf(t, a, ref) != nil {
		t.Fatalf("a parked worker entered retirement: %+v", fake.requests)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Park, st.Agents[0].Done = nil, true
		st.Starts[0].Execution.Completed = true
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	tick(t, a, w)
	tick(t, a, w)
	if len(fake.requests) != 1 || fake.requests[0].Trigger != execution.RetireTriggerReport {
		t.Fatalf("retire requests = %+v", fake.requests)
	}
	x := retirementOf(t, a, ref)
	if !x.Finished() || x.Stage != execution.RetireRetired || x.RetainUntil.IsZero() || x.Attempts != 0 {
		t.Fatalf("retirement = %+v", x)
	}
}

// An unreachable host neither blocks revocation nor reads as retired, and the
// retry resumes the retirement already recorded.
func TestExecutionRetirementSurvivesAnUnreachableHost(t *testing.T) {
	a, w, fake, ref := retireFixture(t, func(_ *state.State, r *state.ExecutionRecord) { r.Request.GitHub = true })
	fake.fail = &execution.UnreachableError{Operation: "retire", Cause: errors.New("connection refused")}
	tick(t, a, w)
	x := retirementOf(t, a, ref)
	if x.RevokedAt.IsZero() || !x.RetiredAt.IsZero() || x.HostError == "" || x.Attempts != 1 || !x.NextAttempt.After(w.now) || x.Finished() {
		t.Fatalf("retirement = %+v", x)
	}
	tick(t, a, w)
	if len(fake.requests) != 1 {
		t.Fatalf("a retry ignored its backoff: %d", len(fake.requests))
	}
	fake.fail = nil
	w.now = x.NextAttempt.Add(time.Second)
	tick(t, a, w)
	done := retirementOf(t, a, ref)
	if !done.Finished() || done.ID != x.ID || done.HostError != "" || !done.RevokedAt.Equal(x.RevokedAt) {
		t.Fatalf("resumed retirement = %+v", done)
	}
	if len(fake.requests) != 2 || fake.requests[1].ID != x.ID {
		t.Fatalf("retire requests = %+v", fake.requests)
	}
}

// A failed revocation is retried on its own and never reported as done, while
// the host's part does not wait for GitHub.
func TestExecutionRevocationRetriesWithoutFalseSuccess(t *testing.T) {
	a, w, fake, ref := retireFixture(t, func(_ *state.State, r *state.ExecutionRecord) { r.Request.GitHub = true })
	ledger := a.workerGitHubPath(ref)
	if err := os.MkdirAll(filepath.Dir(ledger), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger, []byte("not a ledger"), 0o600); err != nil {
		t.Fatal(err)
	}
	tick(t, a, w)
	x := retirementOf(t, a, ref)
	if !x.RevokedAt.IsZero() || x.RevokeError == "" || x.RetiredAt.IsZero() || x.Finished() || x.Attempts != 1 {
		t.Fatalf("retirement = %+v", x)
	}
	if err := os.WriteFile(ledger, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.now = x.NextAttempt.Add(time.Second)
	tick(t, a, w)
	done := retirementOf(t, a, ref)
	if !done.Finished() || done.RevokeError != "" || len(fake.requests) != 1 {
		t.Fatalf("retirement = %+v, requests %d", done, len(fake.requests))
	}
}

// A stop its host never acknowledged revokes at once; the host's part waits.
func TestExecutionStopRevokesBeforeItsHostAnswers(t *testing.T) {
	a, w, fake, ref := retireFixture(t, func(_ *state.State, r *state.ExecutionRecord) {
		r.Request.GitHub = true
		r.Observation.ProcessState = execution.Running
		r.StopRequest = &execution.StopRequest{ID: retireFixtureStop, Reason: "operator stop"}
	})
	tick(t, a, w)
	x := retirementOf(t, a, ref)
	if x.RevokedAt.IsZero() || !x.RetiredAt.IsZero() || x.Trigger != execution.RetireTriggerStop || len(fake.requests) != 0 {
		t.Fatalf("retirement = %+v, requests %d", x, len(fake.requests))
	}
}

func TestExecutionHostRetirementWaits(t *testing.T) {
	for name, change := range map[string]func(*state.State, *state.ExecutionRecord){
		"held by a stop":     func(st *state.State, _ *state.ExecutionRecord) { st.Stop = &state.Stop{Reason: "person's stop"} },
		"transcript pending": func(_ *state.State, r *state.ExecutionRecord) { r.TranscriptCollected = false },
		"publication pending": func(_ *state.State, r *state.ExecutionRecord) {
			r.PendingPublication = &state.ExecutionPendingPublication{Head: "0123456789012345678901234567890123456789", BundleDigest: execution.Digest([]byte("bundle"))}
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, w, fake, ref := retireFixture(t, func(st *state.State, r *state.ExecutionRecord) {
				r.Request.GitHub = true
				change(st, r)
			})
			tick(t, a, w)
			x := retirementOf(t, a, ref)
			if len(fake.requests) != 0 || x.RevokedAt.IsZero() || !x.RetiredAt.IsZero() {
				t.Fatalf("retirement = %+v, requests %d", x, len(fake.requests))
			}
		})
	}
}

// A reservation no start left the coordinator for holds nothing on its host.
func TestExecutionUnsentReservationRetiresWithoutItsHost(t *testing.T) {
	a, w, fake, ref := retireFixture(t, func(st *state.State, r *state.ExecutionRecord) {
		st.Agents = nil
		r.Phase, r.StartUnsent, r.ReleasedAt = executionPending, true, time.Now()
	})
	tick(t, a, w)
	x := retirementOf(t, a, ref)
	if !x.Finished() || x.Stage != executionNothingOnHost || len(fake.requests) != 0 {
		t.Fatalf("retirement = %+v, requests %d", x, len(fake.requests))
	}
}

// A host answering anything but a finished retirement is not confirmed.
func TestExecutionRetirementRefusesAnUnconfirmedAnswer(t *testing.T) {
	a, w, fake, ref := retireFixture(t, nil)
	fake.fail = errors.New("execution is unfinished")
	tick(t, a, w)
	x := retirementOf(t, a, ref)
	if !x.RetiredAt.IsZero() || x.HostError == "" {
		t.Fatalf("retirement = %+v", x)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if rec, _ := executionRecord(st, ref); rec.Observation.Reachability != execution.Reachable || rec.Observation.LastError != "" {
		t.Fatalf("a host refusal changed the observation: %+v", rec.Observation)
	}
}

// A host on an older release holds the retirement open on its backoff without
// reporting it as retired.
func TestExecutionRetirementWaitsForAnOlderHost(t *testing.T) {
	a, w, fake, ref := retireFixture(t, nil)
	fake.fail = execution.ErrUnsupported
	tick(t, a, w)
	x := retirementOf(t, a, ref)
	if !x.RetiredAt.IsZero() || x.HostError == "" || !x.NextAttempt.After(w.now) {
		t.Fatalf("retirement = %+v", x)
	}
	if w.isActive(executionRetireEmitKey) {
		t.Fatal("an older host was raised as a failure")
	}
}

// A backlog retires in bounded ticks, the rest on the next one.
func TestExecutionRetirementBoundsATick(t *testing.T) {
	a, w, fake, _ := retireFixture(t, func(st *state.State, _ *state.ExecutionRecord) {
		first := st.Starts[0]
		st.Agents = nil
		for range executionRetireBatch {
			next := first
			copied := *first.Execution
			copied.Handle.Ref.Execution = execution.ExecutionID(uuid.NewString())
			next.Execution = &copied
			st.Starts = append(st.Starts, next)
		}
	})
	tick(t, a, w)
	if len(fake.requests) != executionRetireBatch {
		t.Fatalf("first tick retired %d", len(fake.requests))
	}
	tick(t, a, w)
	if len(fake.requests) != executionRetireBatch+1 {
		t.Fatalf("second tick left a total of %d", len(fake.requests))
	}
}
