package cmd

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A start that fails before any request reached its host (here: a legacy
// host's untyped inspection error) holds no host or desk slot: the host's
// maxBusy never fills with reservations that run nothing.
func TestExecutionStartFailingBeforeTheHostReleasesItsReservation(t *testing.T) {
	a, brief := sshStartTestApp(t)
	sent := 0
	fake := &testExecutionTransport{}
	fake.start = func(context.Context, execution.StartRequest) (execution.Handle, error) {
		sent++
		return execution.Handle{}, errors.New("fixture start reached the host")
	}
	fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
		return execution.Observation{}, errors.New("lstat journal: no such file or directory")
	}
	legacy := &sourceRecoveryTransport{testExecutionTransport: fake, upload: func(context.Context, execution.Ref, []byte) error {
		sent++
		return errors.New("fixture upload reached the host")
	}}
	a.executionTransport = func(execution.Host) execution.Transport { return legacy }
	maxBusy := a.cfg.Agents.Hosts[sshTestSelector].MaxBusy
	for n := range maxBusy + 1 {
		_, err := runAgents(a, agentStartName, "--name", "remote-worker", brief, "--host", sshTestSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--model", bgModel)
		if err == nil || !strings.Contains(err.Error(), "cannot reconcile pending preparation") || !strings.Contains(err.Error(), "released") {
			t.Fatalf("start %d: %v, want the preparation failure and its release", n, err)
		}
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if sent != 0 {
		t.Fatalf("%d requests reached the host", sent)
	}
	if busy := executionHostBusy(st, sshTestSelector); busy != 0 {
		t.Fatalf("host busy = %d, want 0", busy)
	}
	if c := countAgents(st, nil, a.now); len(c.Busy) != 0 || len(st.Agents) != 0 {
		t.Fatalf("desk busy %v, roster %d: a released reservation still counts", c.Busy, len(st.Agents))
	}
	for _, s := range st.Starts {
		if r := s.Execution; r == nil || r.Released.IsZero() || !r.Terminal() || executionNeedsSync(st, r) || !strings.Contains(r.Release, "cannot reconcile") {
			t.Fatalf("record not released: %+v", r)
		}
	}
}

// A start whose request may have reached the host keeps its reservation:
// only the host's confirmation releases it.
func TestExecutionStartFailingAfterTheHostKeepsItsReservation(t *testing.T) {
	a, brief := sshStartTestApp(t)
	fake := &testExecutionTransport{}
	fake.start = func(context.Context, execution.StartRequest) (execution.Handle, error) {
		return execution.Handle{}, &execution.UnreachableError{Operation: agentStartName, Cause: errors.New("fixture reply lost")}
	}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	if _, err := runAgents(a, agentStartName, "--name", "remote-worker", brief, "--host", sshTestSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--model", bgModel); err == nil {
		t.Fatal("lost start reply succeeded")
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if executionHostBusy(st, sshTestSelector) != 1 || len(countAgents(st, nil, a.now).Busy) != 1 || st.Starts[0].Execution.Unsent {
		t.Fatal("a start that may have reached the host lost its reservation")
	}
}

func pendingExecution(t *testing.T, a *app, ref execution.Ref, unsent bool, reserved time.Time) {
	t.Helper()
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		rec, _ := executionRecord(st, ref)
		rec.Phase, rec.Unsent, rec.ReservedAt = executionPending, unsent, reserved
		rec.Observation = execution.Observation{ProcessState: execution.Unknown, Reachability: execution.Unreachable, LastError: "fixture start failed"}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// The watch releases a pending reservation past its grace that never
// started: nothing was sent, or the host confirms it has no journal. An
// ambiguous inspection, or a reservation inside the grace, keeps its slots.
func TestExecutionWatchReleasesStaleNeverStartedReservations(t *testing.T) {
	for _, c := range []struct {
		name     string
		unsent   bool
		age      time.Duration
		inspect  error
		released bool
	}{
		{name: "unsent", unsent: true, age: time.Hour, inspect: errors.New("must not be inspected"), released: true},
		{name: "confirmed absent", age: time.Hour, inspect: execution.ErrNotFound, released: true},
		{name: "ambiguous", age: time.Hour, inspect: errors.New("lstat journal: no such file or directory")},
		{name: "unreachable", age: time.Hour, inspect: &execution.UnreachableError{Operation: sshTestInspect, Cause: errors.New("connection lost")}},
		{name: "inside grace", unsent: true, age: time.Minute, inspect: execution.ErrNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, r := executionTestApp(t)
			pendingExecution(t, a, r.Ref, c.unsent, a.now.Add(-c.age))
			inspects := 0
			fake := &testExecutionTransport{
				sync: func(context.Context, execution.Ref) (execution.ControlResult, error) {
					return execution.ControlResult{}, nil
				},
				inspect: func(context.Context, execution.Ref) (execution.Observation, error) {
					inspects++
					return execution.Observation{}, c.inspect
				},
			}
			a.executionTransport = func(execution.Host) execution.Transport { return fake }
			w := &watcher{app: a}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			st = w.syncExecutions(context.Background(), st)
			rec, _ := executionRecord(st, r.Ref)
			if got := !rec.Released.IsZero(); got != c.released {
				t.Fatalf("released = %v, want %v (%+v)", got, c.released, rec)
			}
			if c.unsent && c.released && inspects != 0 {
				t.Fatal("the watch contacted the host for a reservation it never sent")
			}
			busy := executionHostBusy(st, sshTestSelector)
			if c.released {
				if busy != 0 || len(st.Agents) != 0 || len(st.Records) != 0 || executionNeedsSync(st, rec) {
					t.Fatalf("released reservation still holds: host busy %d, roster %d, records %d", busy, len(st.Agents), len(st.Records))
				}
				return
			}
			if busy != 1 || len(countAgents(st, nil, a.now).Busy) != 1 {
				t.Fatal("an unconfirmed reservation lost its slots")
			}
		})
	}
}

// agents recover of a reservation the host confirms it holds nothing of
// binds it to the host's current policy, rather than retry a request the
// host now refuses.
func TestExecutionRecoverRebindsNeverStartedReservationToCurrentPolicy(t *testing.T) {
	rig := newPublicationRig(t)
	a := rig.a
	pendingExecution(t, a, rig.request.Ref, false, a.now)
	host := a.cfg.Agents.Hosts[sshTestSelector]
	host.PolicyDigest = execution.Digest([]byte("replaced fixture policy"))
	a.cfg.Agents.Hosts[sshTestSelector] = host
	handle := execution.Handle{Ref: rig.request.Ref, Repository: rig.request.Repository, Commit: rig.base, Workspace: filepath.Join(host.WorkspaceRoot, string(rig.request.Ref.Execution), "workspace"), Unit: execution.UnitName(rig.request.Ref), ConversationID: rig.request.ConversationID}
	prepared, running := false, false
	fake := &testExecutionTransport{}
	fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
		if !prepared {
			return execution.Observation{}, execution.ErrNotFound
		}
		phase := execution.Prepared
		if running {
			phase = execution.Running
		}
		return execution.Observation{Handle: handle, ProcessState: phase, Ready: running}, nil
	}
	fake.start = func(_ context.Context, r execution.StartRequest) (execution.Handle, error) {
		if r.PolicyDigest != host.PolicyDigest {
			return execution.Handle{}, errors.New("execution host policy does not match the request")
		}
		prepared = true
		if r.RegistrationReceipt != "" {
			running = true
		}
		return handle, nil
	}
	transport := &sourceRecoveryTransport{testExecutionTransport: fake, upload: func(context.Context, execution.Ref, []byte) error { return nil }}
	a.executionTransport = func(execution.Host) execution.Transport { return transport }
	a.out = &bytes.Buffer{}
	if _, err := runAgents(a, loopRecover, rig.request.Name); err != nil {
		t.Fatalf("recover: %v", err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := executionRecord(st, rig.request.Ref)
	if !running || rec.Request.PolicyDigest != host.PolicyDigest || rec.RequestDigest != rec.Request.Digest() || !rec.Observation.Ready {
		t.Fatal("recovery did not rebind the never-started reservation to the current policy")
	}
}
