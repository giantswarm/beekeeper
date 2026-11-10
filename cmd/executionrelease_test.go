package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

// startUnacknowledged starts name on the fixture host, a start fake fails,
// and returns its recorded reference.
func startUnacknowledged(t *testing.T, a *app, brief, name string) execution.Ref {
	t.Helper()
	if _, err := runAgents(a, agentStartName, "--name", name, brief, "--host", sshTestSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--model", bgModel); err == nil {
		t.Fatalf("%s: failed start reported success", name)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range st.Starts {
		if s.Execution != nil && s.Name == name {
			return s.Execution.Handle.Ref
		}
	}
	t.Fatalf("%s: no reservation recorded", name)
	return execution.Ref{}
}

func fixtureHandle(a *app, r execution.StartRequest) execution.Handle {
	return execution.Handle{Ref: r.Ref, Repository: r.Repository, Commit: strings.Repeat("a", 40), Workspace: path.Join(a.cfg.Agents.Hosts[sshTestSelector].WorkspaceRoot, string(r.Ref.Execution), "workspace"), Unit: execution.UnitName(r.Ref), ConversationID: r.ConversationID}
}

func notFound(context.Context, execution.Ref) (execution.Observation, error) {
	return execution.Observation{}, execution.ErrNotFound
}

// A start that fails before its host acknowledged it holds no host or desk
// slot past its failure: more failed starts than the host's maxBusy leave
// the host's room and the roster as they were.
func TestExecutionFailedUnacknowledgedStartsReleaseTheirSlots(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fake  func(sends *int) execution.Transport
		sends int
	}{
		{name: "host refused the start", sends: 1, fake: func(sends *int) execution.Transport {
			return &testExecutionTransport{inspect: notFound, start: func(context.Context, execution.StartRequest) (execution.Handle, error) {
				*sends++
				return execution.Handle{}, errors.New("execution policy digest mismatch")
			}}
		}},
		{name: "coordinator aborted before the start", fake: func(sends *int) execution.Transport {
			fake := &testExecutionTransport{
				inspect: func(context.Context, execution.Ref) (execution.Observation, error) {
					return execution.Observation{}, errors.New("lstat: no such file or directory")
				},
				start: func(context.Context, execution.StartRequest) (execution.Handle, error) {
					*sends++
					return execution.Handle{}, errors.New("start sent after an aborted preparation")
				},
			}
			return &sourceRecoveryTransport{testExecutionTransport: fake, upload: func(context.Context, execution.Ref, []byte) error {
				return errors.New("unsupported source upload")
			}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, brief := sshStartTestApp(t)
			gitBefore := executionPublishGit
			t.Cleanup(func() { executionPublishGit = gitBefore })
			executionPublishGit = func(context.Context, string, ...string) ([]byte, error) {
				return nil, errors.New("fixture source fetch refused")
			}
			sends := 0
			fake := tc.fake(&sends)
			a.executionTransport = func(execution.Host) execution.Transport { return fake }
			host := a.cfg.Agents.Hosts[sshTestSelector]
			for n := range host.MaxBusy + 1 {
				ref := startUnacknowledged(t, a, brief, fmt.Sprintf("worker-%d", n))
				st, err := a.store.Read()
				if err != nil {
					t.Fatal(err)
				}
				rec, _ := executionRecord(st, ref)
				if !rec.Released() || !rec.Terminal() || executionHostBusy(st, host.Identity) != 0 || len(countAgents(st, nil, a.now, a.cfg).Busy) != 0 || len(st.Agents) != 0 || len(st.Records) != 0 {
					t.Fatalf("start %d: the unacknowledged reservation kept a host or desk slot", n)
				}
				if executionNeedsSync(st, rec) {
					t.Fatal("a released reservation is still reconciled with its host")
				}
				if err := a.activateExecution(context.Background(), ref); Code(err) != ExitRefused || !strings.Contains(err.Error(), "released") {
					t.Fatalf("recovering a released reservation: %v", err)
				}
			}
			if sends != tc.sends*(host.MaxBusy+1) {
				t.Fatalf("start requests sent = %d", sends)
			}
		})
	}
}

// A lost start reply may have started the run: its reservation stays for
// recovery, and only the host's confirmed absence releases it.
func TestExecutionLostStartReplyKeepsItsReservation(t *testing.T) {
	a, brief := sshStartTestApp(t)
	fake := &testExecutionTransport{inspect: notFound, start: func(context.Context, execution.StartRequest) (execution.Handle, error) {
		return execution.Handle{}, &execution.UnreachableError{Operation: agentStartName, Cause: errors.New("fixture reply lost")}
	}}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	ref := startUnacknowledged(t, a, brief, "lost-reply")
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := executionRecord(st, ref)
	if rec.Released() || rec.StartUnsent || rec.StartSentAt.IsZero() || executionHostBusy(st, ref.Host) != 1 || len(st.Agents) != 1 {
		t.Fatal("a lost start reply released its reservation")
	}
}

// The loop's host sync releases a reservation its host never acknowledged
// once its start window passed and the host holds no journal of it; inside
// the window, or with a host journal, it stays.
func TestExecutionSyncReleasesStaleUnacknowledgedReservation(t *testing.T) {
	a, brief := sshStartTestApp(t)
	var journal *execution.Handle
	fake := &testExecutionTransport{start: func(context.Context, execution.StartRequest) (execution.Handle, error) {
		return execution.Handle{}, &execution.UnreachableError{Operation: agentStartName, Cause: errors.New("fixture reply lost")}
	}}
	fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
		if journal == nil {
			return execution.Observation{}, execution.ErrNotFound
		}
		return execution.Observation{Handle: *journal, ProcessState: execution.Prepared}, nil
	}
	stops := 0
	fake.stop = func(context.Context, execution.Ref, execution.StopRequest) (execution.Ack, error) {
		stops++
		return execution.Ack{}, nil
	}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	ref := startUnacknowledged(t, a, brief, "stale-worker")
	reserved := func() *state.ExecutionRecord {
		t.Helper()
		st, err := a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		rec, _ := executionRecord(st, ref)
		return rec
	}
	_ = a.syncExecutionHost(context.Background(), sshTestSelector)
	if reserved().Released() {
		t.Fatal("a reservation inside its start window was released")
	}
	age := func() {
		t.Helper()
		if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			rec, _ := executionRecord(st, ref)
			rec.ReservedAt = rec.ReservedAt.Add(-2 * unstartedReleaseAfter)
			rec.StartSentAt = rec.StartSentAt.Add(-2 * unstartedReleaseAfter)
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	age()
	rec := reserved()
	h := fixtureHandle(a, rec.Request)
	journal = &h
	_ = a.syncExecutionHost(context.Background(), sshTestSelector)
	if reserved().Released() || stops != 1 {
		t.Fatalf("a reservation its host holds a journal of was released, or not stopped (%d stops)", stops)
	}
	journal = nil
	if err := a.syncExecutionHost(context.Background(), sshTestSelector); err != nil {
		t.Fatalf("sync: %v", err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if rec, _ := executionRecord(st, ref); !rec.Released() || executionHostBusy(st, ref.Host) != 0 || len(st.Agents) != 0 || len(countAgents(st, nil, a.now, a.cfg).Busy) != 0 {
		t.Fatal("the stale unacknowledged reservation kept its slots")
	}
}

// A start whose activation timed out says what it left behind, and the same
// start activates that execution instead of failing on its reserved name.
func TestTimedOutActivationIsReportedAndRetried(t *testing.T) {
	a, brief := sshStartTestApp(t)
	var handle execution.Handle
	starts, ready := 0, false
	fake := &testExecutionTransport{}
	fake.start = func(_ context.Context, r execution.StartRequest) (execution.Handle, error) {
		handle = fixtureHandle(a, r)
		if r.RegistrationReceipt == "" {
			return handle, nil
		}
		starts++
		if starts == 1 {
			return execution.Handle{}, &execution.UnreachableError{Operation: agentStartName, Cause: context.DeadlineExceeded}
		}
		ready = true
		return handle, nil
	}
	fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
		if ready {
			return execution.Observation{Handle: handle, ProcessState: execution.Running, Ready: true}, nil
		}
		return execution.Observation{Handle: handle, ProcessState: execution.Prepared}, nil
	}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	options := executionStartOptions{Host: sshTestSelector, Repository: sshTestRepository, GitRef: mainBranch, Harness: claudeComm, Model: bgModel}
	err := a.startSSHExecution(t.Context(), "timed-out-worker", brief, options)
	if err == nil || !strings.Contains(err.Error(), "exists on its host, prepared, and no process runs") {
		t.Fatalf("timed-out activation = %v", err)
	}
	if err := a.startSSHExecution(t.Context(), "timed-out-worker", brief, options); err != nil {
		t.Fatalf("the same start did not reconcile: %v", err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if starts != 2 || len(st.Starts) != 1 {
		t.Fatalf("retry reserved anew: %d activations, %d records", starts, len(st.Starts))
	}
}

// The caller's own deadline can be what failed the activation: once the host
// answers that the process runs without a stop request, the start completes;
// a stop request on either post-activation inspection fails safe.
func TestActivationReconciledAfterTheCallersDeadline(t *testing.T) {
	for _, tc := range []struct {
		name               string
		stopOnInspectAfter int
		wantErr            bool
		wantInspections    int
	}{
		{name: "ready running", wantInspections: 2},
		{name: "stop requested on reconciliation", stopOnInspectAfter: 1, wantErr: true, wantInspections: 1},
		{name: "stop requested on acceptance", stopOnInspectAfter: 2, wantErr: true, wantInspections: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, brief := sshStartTestApp(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var handle execution.Handle
			launched, activations, postChecks := false, 0, 0
			fake := &testExecutionTransport{}
			fake.start = func(_ context.Context, r execution.StartRequest) (execution.Handle, error) {
				handle = fixtureHandle(a, r)
				if r.RegistrationReceipt == "" {
					return handle, nil
				}
				activations++
				launched = true
				cancel()
				return execution.Handle{}, &execution.UnreachableError{Operation: agentStartName, Cause: context.DeadlineExceeded}
			}
			fake.inspect = func(ctx context.Context, _ execution.Ref) (execution.Observation, error) {
				if err := ctx.Err(); err != nil {
					return execution.Observation{}, err
				}
				if launched {
					postChecks++
					stop := tc.stopOnInspectAfter > 0 && postChecks >= tc.stopOnInspectAfter
					return execution.Observation{Handle: handle, ProcessState: execution.Running, Ready: true, StopRequested: stop}, nil
				}
				return execution.Observation{Handle: handle, ProcessState: execution.Prepared}, nil
			}
			a.executionTransport = func(execution.Host) execution.Transport { return fake }
			options := executionStartOptions{Host: sshTestSelector, Repository: sshTestRepository, GitRef: mainBranch, Harness: claudeComm, Model: bgModel}
			err := a.startSSHExecution(ctx, "reconciled-worker", brief, options)
			if tc.wantErr && Code(err) != ExitRefused {
				t.Fatalf("stop-requested start did not fail safe: %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("a confirmed start failed on the caller's spent deadline: %v", err)
			}
			if activations != 1 || postChecks != tc.wantInspections {
				t.Fatalf("activations=%d postChecks=%d, want 1 and %d", activations, postChecks, tc.wantInspections)
			}
		})
	}
}

// A start that timed out and that nobody retried stops holding its host: past
// the start's window the sync stops it; a start inside the window stays.
func TestExecutionSyncStopsAStartThatNeverActivated(t *testing.T) {
	a, brief := sshStartTestApp(t)
	var stops []execution.StopRequest
	var handle execution.Handle
	fake := &testExecutionTransport{}
	fake.start = func(_ context.Context, r execution.StartRequest) (execution.Handle, error) {
		handle = fixtureHandle(a, r)
		if r.RegistrationReceipt == "" {
			return handle, nil
		}
		return execution.Handle{}, &execution.UnreachableError{Operation: agentStartName, Cause: context.DeadlineExceeded}
	}
	fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
		return execution.Observation{Handle: handle, ProcessState: execution.Prepared}, nil
	}
	fake.stop = func(_ context.Context, _ execution.Ref, q execution.StopRequest) (execution.Ack, error) {
		stops = append(stops, q)
		return execution.Ack{}, nil
	}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	options := executionStartOptions{Host: sshTestSelector, Repository: sshTestRepository, GitRef: mainBranch, Harness: claudeComm, Model: bgModel}
	if err := a.startSSHExecution(t.Context(), "unactivated-worker", brief, options); err == nil {
		t.Fatal("a timed-out activation reported success")
	}
	_ = a.syncExecutionHost(t.Context(), sshTestSelector)
	if len(stops) != 0 {
		t.Fatalf("a start inside its window was stopped: %+v", stops)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.StartSentAt = st.Starts[0].Execution.StartSentAt.Add(-2 * unstartedReleaseAfter)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = a.syncExecutionHost(t.Context(), sshTestSelector)
	if len(stops) != 1 || !strings.Contains(stops[0].Reason, "never activated") {
		t.Fatalf("stale prepared start not stopped: %+v", stops)
	}
}

// agents recover binds a reservation its host never acknowledged to the
// host's current policy before it sends the start; one the host holds a
// journal of keeps its request.
func TestExecutionRecoverRebindsUnstartedReservationToCurrentPolicy(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprintf("host journal %v", held), func(t *testing.T) {
			a, brief := sshStartTestApp(t)
			lost := true
			running := false
			var handle execution.Handle
			fake := &testExecutionTransport{}
			fake.start = func(_ context.Context, r execution.StartRequest) (execution.Handle, error) {
				if lost {
					lost = false
					return execution.Handle{}, &execution.UnreachableError{Operation: agentStartName, Cause: errors.New("fixture reply lost")}
				}
				if r.PolicyDigest != a.cfg.Agents.Hosts[sshTestSelector].PolicyDigest {
					return execution.Handle{}, errors.New("execution policy digest mismatch")
				}
				handle = fixtureHandle(a, r)
				if r.RegistrationReceipt != "" {
					running = true
				}
				return handle, nil
			}
			fake.inspect = func(_ context.Context, ref execution.Ref) (execution.Observation, error) {
				if handle.Ref.Execution == "" && !held {
					return execution.Observation{}, execution.ErrNotFound
				}
				if handle.Ref.Execution == "" {
					st, _ := a.store.Read()
					rec, _ := executionRecord(st, ref)
					return execution.Observation{Handle: fixtureHandle(a, rec.Request), ProcessState: execution.Prepared}, nil
				}
				state := execution.Prepared
				if running {
					state = execution.Running
				}
				return execution.Observation{Handle: handle, ProcessState: state, Ready: running}, nil
			}
			a.executionTransport = func(execution.Host) execution.Transport { return fake }
			ref := startUnacknowledged(t, a, brief, "rebound-worker")
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			old, _ := executionRecord(st, ref)
			oldDigest := old.RequestDigest
			// The host was prepared again: its policy changed.
			host := a.cfg.Agents.Hosts[sshTestSelector]
			host.PolicyDigest = execution.Digest([]byte("re-prepared fixture policy"))
			a.cfg.Agents.Hosts[sshTestSelector] = host
			err = a.activateExecution(context.Background(), ref)
			st, readErr := a.store.Read()
			if readErr != nil {
				t.Fatal(readErr)
			}
			rec, _ := executionRecord(st, ref)
			if held {
				if Code(err) != ExitRefused || rec.RequestDigest != oldDigest || rec.Request.PolicyDigest == host.PolicyDigest || rec.Released() {
					t.Fatalf("a reservation its host holds was rebound or released: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("recover: %v", err)
			}
			if rec.Request.PolicyDigest != host.PolicyDigest || rec.RequestDigest != rec.Request.Digest() || rec.RequestDigest == oldDigest || !rec.Observation.Ready {
				t.Fatal("recover did not rebind the reservation to the host's current policy")
			}
		})
	}
}

// lostReplyReservation records a reservation whose start reply was lost and
// rewinds it to the given send state: unsent (mid-preparation) or sent at.
func lostReplyReservation(t *testing.T, a *app, brief string, fake *testExecutionTransport, unsent bool, sentAgo time.Duration) execution.Ref {
	t.Helper()
	fake.start = func(context.Context, execution.StartRequest) (execution.Handle, error) {
		return execution.Handle{}, &execution.UnreachableError{Operation: agentStartName, Cause: errors.New("fixture reply lost")}
	}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	ref := startUnacknowledged(t, a, brief, "rewound-worker")
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		rec, _ := executionRecord(st, ref)
		rec.StartUnsent = unsent
		rec.Observation = execution.Observation{ProcessState: execution.Unknown, Reachability: execution.Unknown}
		rec.ReservedAt = rec.ReservedAt.Add(-sentAgo)
		rec.StartSentAt = time.Time{}
		if !unsent {
			rec.StartSentAt = time.Now().Add(-sentAgo)
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	return ref
}

// A stop of a reservation its host never acknowledged releases it instead
// of asking the host, which holds no journal of it; a released one is not
// stopped again, and a start sent inside its window waits for it to settle.
func TestExecutionStopReleasesUnacknowledgedReservation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		unsent   bool
		sentAgo  time.Duration
		released bool
	}{
		{name: "start never sent", unsent: true, released: true},
		{name: "sent start past its window", sentAgo: 2 * unstartedReleaseAfter, released: true},
		{name: "sent start inside its window", sentAgo: time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, brief := sshStartTestApp(t)
			stops := 0
			fake := &testExecutionTransport{inspect: notFound, stop: func(context.Context, execution.Ref, execution.StopRequest) (execution.Ack, error) {
				stops++
				return execution.Ack{}, errors.New("open journal: no such file or directory")
			}}
			ref := lostReplyReservation(t, a, brief, fake, tc.unsent, tc.sentAgo)
			err := a.stopExecution(context.Background(), ref, "fixture release stop")
			st, readErr := a.store.Read()
			if readErr != nil {
				t.Fatal(readErr)
			}
			rec, _ := executionRecord(st, ref)
			if stops != 0 || rec.Observation.Reachability == execution.Unreachable {
				t.Fatal("the stop asked a host that holds no journal of the reservation")
			}
			if !tc.released {
				if Code(err) != ExitRefused || rec.Released() || rec.StopRequest == nil {
					t.Fatalf("a start inside its window was released or lost its stop: %v", err)
				}
				return
			}
			if err != nil || !rec.Released() || len(st.Agents) != 0 || executionHostBusy(st, ref.Host) != 0 {
				t.Fatalf("the stopped reservation was not released: %v", err)
			}
			// beekeeper stop, agents stop and the loop stop a released
			// reservation no more.
			if err := a.stopExecution(context.Background(), ref, "fixture release stop again"); err != nil || stops != 0 {
				t.Fatalf("stopping a released reservation: %v", err)
			}
		})
	}
}

// A stop recorded while the start pins and uploads its source keeps the
// start request from leaving: nothing reaches the host, and the
// reservation is released.
func TestExecutionStopDuringPreparationSendsNoStart(t *testing.T) {
	for _, desk := range []bool{false, true} {
		t.Run(fmt.Sprintf("desk stop %v", desk), func(t *testing.T) {
			a, brief := sshStartTestApp(t)
			gitBefore := executionPublishGit
			t.Cleanup(func() { executionPublishGit = gitBefore })
			executionPublishGit = func(_ context.Context, _ string, args ...string) ([]byte, error) {
				switch args[0] {
				case "rev-parse":
					return []byte(strings.Repeat("b", 40) + "\n"), nil
				case "bundle":
					root, err := os.OpenRoot(filepath.Dir(args[2]))
					if err != nil {
						return nil, err
					}
					defer root.Close() //nolint:errcheck // test fixture
					return nil, root.WriteFile(filepath.Base(args[2]), []byte("fixture bundle"), 0o600)
				}
				return nil, nil
			}
			sends := 0
			fake := &testExecutionTransport{inspect: notFound, start: func(context.Context, execution.StartRequest) (execution.Handle, error) {
				sends++
				return execution.Handle{}, errors.New("start sent after a stop")
			}}
			uploader := &sourceRecoveryTransport{testExecutionTransport: fake, upload: func(_ context.Context, ref execution.Ref, _ []byte) error {
				return a.store.Update(func(st *state.State) ([]state.Event, error) {
					if desk {
						st.Stop = &state.Stop{Reason: "fixture desk stop", At: time.Now()}
						return nil, nil
					}
					rec, _ := executionRecord(st, ref)
					rec.StopRequest = &execution.StopRequest{ID: "fixture-stop", Reason: "fixture release stop"}
					return nil, nil
				})
			}}
			a.executionTransport = func(execution.Host) execution.Transport { return uploader }
			ref := startUnacknowledged(t, a, brief, "stopped-worker")
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			rec, _ := executionRecord(st, ref)
			if sends != 0 || !rec.StartUnsent || !rec.Released() || executionHostBusy(st, ref.Host) != 0 {
				t.Fatal("a stop during preparation let the start leave or kept the reservation")
			}
		})
	}
}

// Host sync asks the host nothing about a reservation whose start never
// left the coordinator: no not-found failure marks it or the host.
func TestExecutionSyncSkipsUnsentReservation(t *testing.T) {
	a, brief := sshStartTestApp(t)
	calls := 0
	fake := &testExecutionTransport{}
	fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
		calls++
		return execution.Observation{}, execution.ErrNotFound
	}
	ref := lostReplyReservation(t, a, brief, fake, true, 0)
	fake.sync = func(context.Context, execution.Ref) (execution.ControlResult, error) {
		calls++
		return execution.ControlResult{}, errors.New("journal not found")
	}
	if err := a.syncExecutionHost(context.Background(), sshTestSelector); err != nil {
		t.Fatalf("sync: %v", err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := executionRecord(st, ref)
	if calls != 0 || rec.Released() || rec.Observation.Reachability == execution.Unreachable || rec.Observation.LastError != "" {
		t.Fatal("host sync contacted the host about an unsent reservation or released it inside its window")
	}
}

// Force admits the coordinator reservation but does not override the host's
// own capacity limits. Explain that boundary before sending preparation and
// release the reservation when the host confirms that no execution exists.
func TestExecutionForcedRemoteCapacityRefusalReleasesReservation(t *testing.T) {
	a, brief := sshStartTestApp(t)
	const block = "1m load 53 over 14"
	probe := a.executionProbe
	a.executionProbe = func(ctx context.Context, h execution.Host) (execution.Capabilities, error) {
		caps, err := probe(ctx, h)
		caps.Resources.Blocks = []string{block}
		return caps, err
	}
	sends := 0
	a.executionTransport = func(execution.Host) execution.Transport {
		return &testExecutionTransport{inspect: notFound, start: func(_ context.Context, r execution.StartRequest) (execution.Handle, error) {
			sends++
			if !strings.Contains(fmt.Sprint(a.out), "--force overrides coordinator capacity only") || !strings.Contains(fmt.Sprint(a.out), "load, disk, build slots (with --builds) and maxBusy") {
				t.Fatal("remote preparation preceded explanation of force limits")
			}
			st, err := a.store.Read()
			if err != nil || executionHostBusy(st, r.Ref.Host) != 1 {
				t.Fatal("forced start did not reserve coordinator capacity before preparation")
			}
			return execution.Handle{}, errors.New("execution host has no room: " + block)
		}}
	}
	out, err := runAgents(a, agentStartName, "--name", "remote-worker", brief, "--host", sshTestSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--model", bgModel, "--force")
	if err == nil || !strings.Contains(err.Error(), block) || !strings.Contains(err.Error(), "reservation is released") || sends != 1 {
		t.Fatalf("forced refusal: sends=%d, output=%s, error=%v", sends, out, err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Starts) != 1 || st.Starts[0].Execution == nil {
		t.Fatal("forced refusal lost its execution history")
	}
	rec := st.Starts[0].Execution
	if !rec.Released() || !rec.Terminal() || executionHostBusy(st, rec.Handle.Ref.Host) != 0 || len(countAgents(st, nil, a.now, a.cfg).Busy) != 0 || len(st.Agents) != 0 || len(st.Records) != 0 {
		t.Fatal("remote capacity refusal kept a reservation, worker, host or desk slot")
	}
}
