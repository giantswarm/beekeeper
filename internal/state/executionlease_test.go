package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/gofrs/flock"
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
