package cmd

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

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
