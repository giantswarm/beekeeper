package execution

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLeaseSignaturesBindExecutionAndRejectReplays(t *testing.T) {
	f := testFixture(t)
	now := f.s.now()
	request := LeaseRequest{Nonce: uuid.NewString()}
	grant, err := IssueLease(f.r.Ref, f.r.Coordinator, nil, request, f.key, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyLease(grant, request, f.r.Ref, f.r.Coordinator, PublicKey(f.key)); err != nil {
		t.Fatal(err)
	}
	replay, err := IssueLease(f.r.Ref, f.r.Coordinator, &grant, request, f.key, now.Add(time.Hour))
	if err != nil || replay != grant {
		t.Fatal("retry extended lease", err)
	}
	stale := LeaseRequest{Nonce: uuid.NewString()}
	if _, err := IssueLease(f.r.Ref, f.r.Coordinator, &grant, stale, f.key, now); err == nil {
		t.Fatal("stale sequence renewed lease")
	}
	for _, mutate := range []func(*LeaseGrant){
		func(g *LeaseGrant) { g.Ref.Generation++ },
		func(g *LeaseGrant) { g.Ref.Host = "other-host" },
		func(g *LeaseGrant) { g.Ref.Execution = ExecutionID(uuid.NewString()) },
		func(g *LeaseGrant) { g.Coordinator = "other-coordinator" },
		func(g *LeaseGrant) { g.Sequence++ },
		func(g *LeaseGrant) { g.ExpiresAt = g.ExpiresAt.Add(time.Second) },
		func(g *LeaseGrant) { g.Nonce = uuid.NewString() },
	} {
		bad := grant
		mutate(&bad)
		if VerifyLease(bad, request, f.r.Ref, f.r.Coordinator, PublicKey(f.key)) == nil {
			t.Fatal("mismatched lease verified")
		}
	}
}

func TestLeaseLostReplyRetriesOriginalGrantAndStopsRevokeRenewal(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	f.r.LeaseRequired = true
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	var issued *LeaseGrant
	calls := 0
	f.s.Callback = func(_ context.Context, _ []string, data []byte) ([]byte, error) {
		var request CallbackRequest
		if err := Decode(data, &request); err != nil {
			return nil, err
		}
		var renewal LeaseRequest
		if err := Decode(request.Payload, &renewal); err != nil {
			return nil, err
		}
		grant, err := IssueLease(f.r.Ref, f.r.Coordinator, issued, renewal, f.key, f.s.now())
		if err != nil {
			return nil, err
		}
		issued = &grant
		calls++
		if calls == 1 {
			return nil, errors.New("lost reply after coordinator commit")
		}
		payload, _ := json.Marshal(grant)
		return json.Marshal(WireResponse{Protocol: Protocol, RequestID: request.RequestID, Ref: request.Ref, Payload: payload})
	}
	if _, err := f.s.renewLease(ctx, f.r.Ref); err == nil {
		t.Fatal("lost reply reported success")
	}
	grant, err := f.s.renewLease(ctx, f.r.Ref)
	if err != nil || grant == nil || grant.Sequence != 1 {
		t.Fatal("lost reply created another grant", err)
	}
	journal, err := f.s.Read(f.r.Ref)
	if err != nil || journal.LeasePending != nil || journal.Lease == nil {
		t.Fatal("grant not durable", err)
	}
	if _, err := f.s.Stop(ctx, f.r.Ref, StopRequest{ID: "stop", Reason: "coordinator lease stop"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.renewLease(ctx, f.r.Ref); err == nil || calls != 2 {
		t.Fatal("stop permitted another renewal")
	}
	if f.launcher.states["personal.service"] != unitActive {
		t.Fatal("personal service changed")
	}
}

func TestLeaseExpiryPersistsStopBeforeTerminationAndReconciles(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	f.r.LeaseRequired = true
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	grant, err := IssueLease(f.r.Ref, f.r.Coordinator, nil, LeaseRequest{Nonce: uuid.NewString()}, f.key, f.s.now())
	if err != nil {
		t.Fatal(err)
	}
	f.launcher.stopError = true
	if err := f.s.expireLease(ctx, f.r.Ref, &grant); err == nil {
		t.Fatal("lost termination acknowledgement hidden")
	}
	journal, err := f.s.Read(f.r.Ref)
	if err != nil || journal.StopRequest == nil || journal.StopAcknowledged {
		t.Fatal("expiry stop intent not durable", err)
	}
	id := journal.StopRequest.ID
	f.launcher.stopError = false
	if err := f.s.expireLease(ctx, f.r.Ref, &grant); err != nil {
		t.Fatal(err)
	}
	observed, err := f.s.Inspect(ctx, f.r.Ref)
	if err != nil || !observed.StopAcknowledged || observed.ProcessState != Exited {
		t.Fatal("termination not reconciled", err)
	}
	journal, _ = f.s.Read(f.r.Ref)
	if journal.StopRequest.ID != id || f.launcher.states["personal.service"] != unitActive || len(f.launcher.starts) != 1 {
		t.Fatal("reconciliation replaced identity or touched personal process")
	}
}

func TestLeaseWatchUsesElapsedClockAndNeverRenewsExpiredAuthority(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	f.r.LeaseRequired = true
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	grant, err := IssueLease(f.r.Ref, f.r.Coordinator, nil, LeaseRequest{Nonce: uuid.NewString()}, f.key, f.s.now())
	if err != nil {
		t.Fatal(err)
	}
	// Coordinator wall time is unrelated to this fake host's elapsed clock.
	acquired := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	ticks := make(chan time.Time, 1)
	ticks <- acquired.Add(LeaseDuration)
	f.s.Callback = func(context.Context, []string, []byte) ([]byte, error) {
		t.Fatal("expired lease renewed")
		return nil, nil
	}
	f.s.leaseLoop(ctx, f.r.Ref, acquired, &grant, ticks, func() time.Time { return acquired.Add(LeaseDuration) })
	j, err := f.s.Read(f.r.Ref)
	if err != nil || j.StopRequest == nil || j.StopAcknowledged {
		t.Fatal("expiry did not retain unacknowledged stop", err)
	}
	if len(f.launcher.stops) != 1 || f.launcher.stops[0] != UnitName(f.r.Ref) {
		t.Fatal("wrong owned unit stopped")
	}
}
