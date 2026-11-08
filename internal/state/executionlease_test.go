package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/execution"
)

func TestExecutionLeaseCommitBypassesDeskReadersAndPreservesOutbox(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref := execution.Ref{Host: "fixture-host", Execution: "fixture-execution", Generation: 1}
	st := &State{Starts: []Start{{Execution: &ExecutionRecord{Request: execution.StartRequest{Ref: ref}}}}, EventOutbox: &EventOutbox{Offset: 0, Data: []byte("{\"verb\":\"fixture\"}\n")}}
	if err := writeJSON(s.path("state.json"), st); err != nil {
		t.Fatal(err)
	}
	reader := flock.New(s.path("state.lock"))
	if err := reader.RLock(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Unlock() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = s.RenewExecutionLease(ctx, ref, func(view *State) (*execution.LeaseGrant, error) {
		// Even an erroneous issuer cannot mutate the saved authority.
		view.Stop = &Stop{Reason: "must not save"}
		return &execution.LeaseGrant{Ref: ref, Sequence: 1}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.load()
	if err != nil || after.EventOutbox == nil || string(after.EventOutbox.Data) != string(st.EventOutbox.Data) || after.Stop != nil || after.Starts[0].Execution.Lease.Sequence != 1 {
		t.Fatal("lease commit changed authority or outbox", err)
	}
}

func TestExecutionLeaseCommitHonorsDeadlineAndWriterFence(t *testing.T) {
	s, err := OpenVersion(t.TempDir(), "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	issue := func(*State) (*execution.LeaseGrant, error) { called = true; return nil, nil }
	if _, err := s.RenewExecutionLease(ctx, execution.Ref{}, issue); err == nil || called {
		t.Fatal("canceled request issued grant")
	}
	if err := writeJSON(s.path("state.json"), &State{Writer: &Writer{Version: "v2.0.0"}}); err != nil {
		t.Fatal(err)
	}
	_, err = s.RenewExecutionLease(context.Background(), execution.Ref{}, issue)
	var stale *StaleWriterError
	if !errors.As(err, &stale) || called {
		t.Fatal("stale writer issued grant", err)
	}
}

func TestExecutionLeaseCommitPreservesCallbackAuthorityEdits(t *testing.T) {
	const (
		cleared = "cleared"
		expired = "expired"
	)
	for _, mode := range []string{"unchanged", "initial-grant", cleared, expired} {
		t.Run(mode, func(t *testing.T) {
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ref := execution.Ref{Host: "fixture-host", Execution: "fixture-execution", Generation: 1}
			initial := &execution.LeaseGrant{Ref: ref, Sequence: 1, ExpiresAt: time.Now().Add(time.Hour)}
			if mode == "initial-grant" {
				initial = nil
			}
			if err := s.Update(func(st *State) ([]Event, error) {
				st.Starts = []Start{{Execution: &ExecutionRecord{Request: execution.StartRequest{Ref: ref}, Lease: initial}}}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			renewed := execution.LeaseGrant{Ref: ref, Sequence: 2, ExpiresAt: time.Now().UTC().Add(2 * time.Hour)}
			if err := s.Update(func(st *State) ([]Event, error) {
				if _, err := s.RenewExecutionLease(context.Background(), ref, func(*State) (*execution.LeaseGrant, error) {
					return &renewed, nil
				}); err != nil {
					return nil, err
				}
				switch mode {
				case cleared:
					st.Starts[0].Execution.Lease = nil
				case expired:
					st.Starts[0].Execution.Lease.ExpiresAt = time.Now().Add(-time.Minute)
				}
				return []Event{{Verb: "test.lease-authority"}}, nil
			}); err != nil {
				t.Fatal(err)
			}
			st, err := s.Read()
			if err != nil {
				t.Fatal(err)
			}
			got := st.Starts[0].Execution.Lease
			switch mode {
			case cleared:
				if got != nil {
					t.Fatal("concurrent renewal restored a cleared lease")
				}
			case expired:
				if got == nil || got.ExpiresAt.After(time.Now()) || got.Sequence != 1 {
					t.Fatal("concurrent renewal overwrote explicit expiry")
				}
			default:
				if got == nil || *got != renewed {
					t.Fatal("unchanged lease lost concurrent grant")
				}
			}
		})
	}
}
