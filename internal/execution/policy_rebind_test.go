package execution

import (
	"context"
	"testing"
)

// A host upgrade re-prepares its policy under a running execution. The unit
// keeps running while its bound policy refuses; only a rebind from exactly the
// bound policy onto exactly the prepared one moves it, through the SSH
// operation, and a stopped execution is never rebound.
func TestPolicyRebindMovesRunningExecutionOntoPreparedPolicy(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	tr := f.transport(t, nil)
	f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	original := f.r.PolicyDigest
	o, err := tr.Inspect(ctx, f.r.Ref)
	if err != nil || o.ProcessState != Running || o.PolicyDigest != original || o.HostPolicyDigest != original || !o.PolicyAvailable() {
		t.Fatalf("running execution does not report its matching policy: %+v, %v", o, err)
	}

	f.s.Version = "upgraded"
	o, err = tr.Inspect(ctx, f.r.Ref)
	if err != nil || o.ProcessState != Running || o.HostPolicyDigest != "" || o.PolicyAvailable() {
		t.Fatalf("stale host policy is not reported as unavailable: %+v, %v", o, err)
	}
	if _, err := tr.RebindPolicy(ctx, f.r.Ref, PolicyRebind{From: original, To: f.s.Policy().Digest()}); err == nil {
		t.Fatal("rebind accepted an unprepared host policy")
	}
	prepared, err := f.s.Prepare(f.s.Policy())
	if err != nil {
		t.Fatal(err)
	}
	if prepared == original {
		t.Fatal("fixture upgrade left the policy unchanged")
	}
	other := Digest([]byte("another policy"))
	for name, q := range map[string]PolicyRebind{
		"wrong source": {From: other, To: prepared},
		"wrong target": {From: original, To: other},
		"no source":    {To: prepared},
	} {
		if _, err := tr.RebindPolicy(ctx, f.r.Ref, q); err == nil {
			t.Fatal("rebind accepted", name)
		}
	}
	if j, err := f.s.Read(f.r.Ref); err != nil || j.BoundPolicy() != original {
		t.Fatal("refused rebind changed the bound policy", err)
	}

	o, err = tr.RebindPolicy(ctx, f.r.Ref, PolicyRebind{From: original, To: prepared})
	if err != nil || o.PolicyDigest != prepared || o.HostPolicyDigest != prepared || !o.PolicyAvailable() || o.ProcessState != Running {
		t.Fatalf("rebind did not bind the prepared policy: %+v, %v", o, err)
	}
	j, err := f.s.Read(f.r.Ref)
	if err != nil || j.BoundPolicy() != prepared || j.Request.PolicyDigest != original || j.Request.Digest() != j.Digest {
		t.Fatal("rebind changed the signed request instead of the journal's bound policy", err)
	}
	if _, err := tr.RebindPolicy(ctx, f.r.Ref, PolicyRebind{From: original, To: prepared}); err != nil {
		t.Fatal("a retried rebind is not idempotent", err)
	}
	if len(f.launcher.starts) != 1 {
		t.Fatal("rebind started another unit")
	}

	if _, err := f.s.Stop(ctx, f.r.Ref, StopRequest{ID: "rebind-stop", Reason: "stopped before the next host upgrade"}); err != nil {
		t.Fatal(err)
	}
	f.s.Version = "upgraded again"
	again, err := f.s.Prepare(f.s.Policy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.RebindPolicy(ctx, f.r.Ref, PolicyRebind{From: prepared, To: again}); err == nil {
		t.Fatal("rebind moved a stopped execution")
	}
}
