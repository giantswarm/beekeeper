package cmd

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestExecutionCoordinatorIssuesGenerationBoundLease(t *testing.T) {
	a, r := executionTestApp(t)
	a.cfg.Identity.Host = r.Coordinator
	var fields map[string]any
	_ = json.Unmarshal(mustExecutionJSON(t, r), &fields)
	fields["leaseRequired"] = true
	if err := json.Unmarshal(mustExecutionJSON(t, fields), &r); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.Request = r
		st.Starts[0].Execution.RequestDigest = r.Digest()
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	var grant json.RawMessage
	if err := coordinatorTestCall(t, a, r, "renew-lease", map[string]any{"previous": 0, "nonce": uuid.NewString()}, &grant, false); err != nil {
		t.Fatalf("registered execution could not acquire coordinator lease: %v", err)
	}
}

func TestExecutionCoordinatorStopRevokesLease(t *testing.T) {
	a, r := executionTestApp(t)
	a.cfg.Identity.Host = r.Coordinator
	r.LeaseRequired = true
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.Request = r
		st.Starts[0].Execution.RequestDigest = r.Digest()
		st.Stop = &state.Stop{Reason: "coordinator lease stop"}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	var grant json.RawMessage
	if err := coordinatorTestCall(t, a, r, "renew-lease", map[string]any{"previous": 0, "nonce": uuid.NewString()}, &grant, false); err == nil {
		t.Fatal("global stop renewed lease")
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Starts[0].Execution.Terminal() {
		t.Fatal("unconfirmed disconnected stop released capacity")
	}
}

// A callback that holds the desk lock cannot consume the lease's renewal
// window. Channels, rather than elapsed sleeps, keep the contention exact.
func TestExecutionCoordinatorLeaseSurvivesBusyDeskAndStopCommit(t *testing.T) {
	a, r := executionTestApp(t)
	r.LeaseRequired = true
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.Request = r
		st.Starts[0].Execution.RequestDigest = r.Digest()
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.store.Update(func(st *state.State) ([]state.Event, error) {
			close(entered)
			<-release
			st.Stop = &state.Stop{Reason: "stop after concurrent renewal"}
			return []state.Event{{Verb: "test.stop"}}, nil
		})
	}()
	<-entered
	// Always unblock the writer even if the renewal fails.
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	var grant execution.LeaseGrant
	request := execution.LeaseRequest{Nonce: uuid.NewString()}
	if err := coordinatorTestCall(t, a, r, "renew-lease", request, &grant, false); err != nil {
		t.Fatal(err)
	}
	if grant.Sequence != 1 || grant.Ref != r.Ref {
		t.Fatal("invalid concurrent grant")
	}
	var retry execution.LeaseGrant
	if err := coordinatorTestCall(t, a, r, "renew-lease", request, &retry, false); err != nil || retry != grant {
		t.Fatal("retry changed grant", err)
	}
	// Read through the commit lock, without queuing on the long callback.
	current, err := a.store.Peek()
	if err != nil || current.Starts[0].Execution.Lease == nil {
		t.Fatal("renewal not durable", err)
	}
}

func TestExecutionCoordinatorStopPreservesConcurrentRenewal(t *testing.T) {
	a, r := executionTestApp(t)
	r.LeaseRequired = true
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.Request = r
		st.Starts[0].Execution.RequestDigest = r.Digest()
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.store.Update(func(st *state.State) ([]state.Event, error) {
			close(entered)
			<-release
			st.Stop = &state.Stop{Reason: "operator desk stop"}
			return nil, nil
		})
	}()
	<-entered
	var grant execution.LeaseGrant
	err := coordinatorTestCall(t, a, r, "renew-lease", execution.LeaseRequest{Nonce: uuid.NewString()}, &grant, false)
	close(release)
	if updateErr := <-done; updateErr != nil {
		t.Fatal(updateErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil || st.Starts[0].Execution.Lease == nil || *st.Starts[0].Execution.Lease != grant {
		t.Fatal("desk commit lost renewed lease", err)
	}
	if err := coordinatorTestCall(t, a, r, "renew-lease", execution.LeaseRequest{Previous: grant.Sequence, Nonce: uuid.NewString()}, &grant, false); err == nil {
		t.Fatal("committed stop permitted renewal")
	}
}
