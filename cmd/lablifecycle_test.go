package cmd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/state"
)

// lifecycleDocker models both nodes of a cluster, preserving them on stop.
func lifecycleDocker(t *testing.T, a *app) (*bool, *int, *int) {
	t.Helper()
	running, starts, stops := true, 0, 0
	a.cfg.Resources = []string{labOne}
	a.labDocker = func(_ context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "ps":
			if !slices.Contains(args, "label=io.x-k8s.kind.cluster="+labCluster) {
				t.Fatalf("not an exact cluster filter: %v", args)
			}
			if slices.Contains(args, "status=running") == running {
				return []byte("node-control\nnode-worker\n"), nil
			}
			return nil, nil
		case labStop:
			if !slices.Equal(args, []string{labStop, "--time", "10", "node-control", "node-worker"}) {
				t.Fatalf("stop: %v", args)
			}
			stops++
			running = false
		case labStart:
			if !slices.Equal(args, []string{labStart, "node-control", "node-worker"}) {
				t.Fatalf("start: %v", args)
			}
			starts++
			running = true
		default:
			t.Fatalf("unexpected Docker mutation: %v", args)
		}
		return nil, nil
	}
	return &running, &starts, &stops
}

func TestLabLifecycleStopsAndClaimRestarts(t *testing.T) {
	a := claimApp(t)
	running, starts, stops := lifecycleDocker(t, a)
	ctx := context.Background()
	r := doctorRun{by: watchParty}
	if lines := a.doctorLabs(ctx, nil, r); len(lines) != 0 {
		t.Fatal(lines)
	}
	a.now = a.now.Add(labIdleGrace - time.Second)
	a.doctorLabs(ctx, nil, r)
	if !*running {
		t.Fatal("stopped before the grace period")
	}
	a.now = a.now.Add(time.Second)
	lines := a.doctorLabs(ctx, nil, doctorRun{by: watchParty, dryRun: true})
	if !*running || len(lines) != 1 || !strings.HasPrefix(lines[0], "would stop kind lab") {
		t.Fatalf("dry run: %v, running %v", lines, *running)
	}
	lines = a.doctorLabs(ctx, nil, r)
	if *running || *stops != 1 || len(lines) != 1 {
		t.Fatalf("stop: %v, running %v, stops %d", lines, *running, *stops)
	}
	if lines := a.doctorLabs(ctx, nil, r); len(lines) != 0 || *stops != 1 {
		t.Fatalf("repeated stop: %v", lines)
	}
	if out, err := runLease(a, "claim", labOne, "-p", "resume proof"); err != nil {
		t.Fatalf("claim: %s: %v", out, err)
	}
	if !*running || *starts != 1 {
		t.Fatalf("claim left nodes stopped: starts %d", *starts)
	}
	// A retry is idempotent and the doctor's old timer cannot stop it.
	if _, err := runLease(a, "claim", labOne, "-p", "resume proof"); err != nil {
		t.Fatal(err)
	}
	a.now = a.now.Add(time.Hour)
	a.doctorLabs(ctx, []*claude.Session{{ID: claimMe.Session}}, r)
	if !*running || *stops != 1 || *starts != 1 {
		t.Fatal("held lab was stopped or restarted twice")
	}
	evs, err := a.store.Events(100, func(e state.Event) bool { return e.Verb == "lab.stop" || e.Verb == "lab.start" })
	if err != nil || len(evs) != 2 {
		t.Fatalf("lifecycle events: %v, %v", evs, err)
	}
}

func TestDoctorReleasesGoneLeaseOnce(t *testing.T) {
	for _, tc := range []struct {
		name        string
		party       state.Party
		live        bool
		wantRelease bool
	}{
		{"gone local", claimOther, false, true},
		{"live local", claimOther, true, false},
		{"gone codex", state.Party{HostSession: "codex_gone", Name: "Codex"}, false, true},
		{"person-held", state.Party{Name: litOperator}, false, false},
		{"remote holder", state.Party{Session: "foreign-worker", Host: "other-host"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := claimApp(t)
			lifecycleDocker(t, a)
			dir := lease.Dir(a.cfg.LeaseDir)
			_, err := dir.Claim(labOne, lease.Holder{Env: labOne, Session: tc.party.Session, HostSession: tc.party.HostSession,
				Host: tc.party.Host, Name: tc.party.Name, Since: a.now.Add(-time.Hour).Format(time.RFC3339)})
			if err != nil {
				t.Fatal(err)
			}
			var sessions []*claude.Session
			if tc.live {
				sessions = []*claude.Session{{ID: tc.party.Session}}
			}
			for range 2 {
				a.doctorLabs(context.Background(), sessions, doctorRun{by: watchParty})
			}
			h, err := dir.Get(labOne)
			if err != nil || (h == nil) != tc.wantRelease {
				t.Fatalf("holder: %v, %v", h, err)
			}
			evs, err := a.store.Events(100, func(e state.Event) bool { return e.Verb == "lease.release-gone" })
			want := 0
			if tc.wantRelease {
				want = 1
			}
			if err != nil || len(evs) != want {
				t.Fatalf("release events: %v, %v", evs, err)
			}
		})
	}
}

func TestLabLifecycleFailureAndRefusal(t *testing.T) {
	a := claimApp(t)
	_, starts, stops := lifecycleDocker(t, a)
	holdLease(t, a, labOne, claimOther)
	if _, err := runLease(a, "claim", labOne, "-p", "not mine"); Code(err) != ExitRefused || *starts != 0 || *stops != 0 {
		t.Fatalf("refused claim touched Docker: %v", err)
	}
	if err := a.resumeLab(context.Background(), labOne); Code(err) != ExitRefused {
		t.Fatalf("non-holder resume: %v", err)
	}
	if err := lease.Dir(a.cfg.LeaseDir).Release(labOne); err != nil {
		t.Fatal(err)
	}
	a.labDocker = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("daemon unavailable") }
	if out, err := runLease(a, "claim", labOne, "-p", "failed restart"); err == nil || strings.Contains(out, "export KUBECONFIG") {
		t.Fatalf("failed restart reported success: %s, %v", out, err)
	}
	h, _ := lease.Dir(a.cfg.LeaseDir).Get(labOne)
	if h == nil || !h.Party().Is(claimMe) {
		t.Fatal("failed restart lost ownership needed for retry")
	}
	argv, err := brokeredLabArgv(sandbox.Request{Resource: labOne, Args: []string{labResume}}, false)
	if err != nil || !slices.Equal(argv, []string{leaseName, labResume, labOne}) {
		t.Fatalf("brokered restart: %v, %v", argv, err)
	}
}

func TestLabLifecycleDryRunAndDockerFailures(t *testing.T) {
	a := claimApp(t)
	_, _, stops := lifecycleDocker(t, a)
	holdLease(t, a, labOne, claimOther)
	ctx := context.Background()
	a.doctorLabs(ctx, nil, doctorRun{by: watchParty, dryRun: true})
	h, _ := lease.Dir(a.cfg.LeaseDir).Get(labOne)
	st, err := a.store.Read()
	if err != nil || h == nil || len(st.Released) != 0 || len(st.LabIdle) != 0 {
		t.Fatalf("dry run changed a lease or timer: %v, %v", h, err)
	}
	if err := lease.Dir(a.cfg.LeaseDir).Release(labOne); err != nil {
		t.Fatal(err)
	}
	working := a.labDocker
	a.labDocker = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("cannot list nodes") }
	a.doctorLabs(ctx, nil, doctorRun{by: watchParty})
	st, err = a.store.Read()
	if err != nil || len(st.LabIdle) != 0 {
		t.Fatal("failed Docker read started an idle timer")
	}
	a.labDocker = working
	a.doctorLabs(ctx, nil, doctorRun{by: watchParty})
	a.now = a.now.Add(labIdleGrace)
	a.labDocker = func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == labStop {
			return nil, errors.New("cannot stop nodes")
		}
		return working(ctx, args...)
	}
	lines := a.doctorLabs(ctx, nil, doctorRun{by: watchParty})
	if *stops != 0 || len(lines) != 1 || !strings.Contains(lines[0], "cannot stop nodes") {
		t.Fatalf("failed stop: %v", lines)
	}
	a.labDocker = working
	a.doctorLabs(ctx, nil, doctorRun{by: watchParty})
	if *stops != 1 {
		t.Fatal("failed stop was not retried")
	}
}

func TestDoctorProtectsClaimAfterSessionSnapshot(t *testing.T) {
	a := claimApp(t)
	lifecycleDocker(t, a)
	a.now = a.now.Add(500 * time.Millisecond)
	// a.now is the doctor's snapshot time; the new claim came afterwards.
	dir := lease.Dir(a.cfg.LeaseDir)
	_, err := dir.Claim(labOne, lease.Holder{Env: labOne, Session: claimOther.Session,
		Since: a.now.Add(time.Millisecond).Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	a.doctorLabs(context.Background(), nil, doctorRun{by: watchParty})
	if h, err := dir.Get(labOne); err != nil || h == nil {
		t.Fatalf("released a claim newer than session snapshot: %v", err)
	}
}

func TestLabLifecycleRestartHonorsClusterLimit(t *testing.T) {
	a := claimApp(t)
	running, starts, _ := lifecycleDocker(t, a)
	*running = false
	a.cfg.MaxKindClusters = 2
	a.kindClusters = func() ([]string, error) { return []string{"other-one", "other-two"}, nil }
	if _, err := runLease(a, "claim", labOne, "-p", "restart at limit"); err == nil || *starts != 0 {
		t.Fatalf("restart exceeded the cluster limit: %v, starts %d", err, *starts)
	}
}

// Handover preserves Since and records the successor before launching it.
// An older process snapshot therefore cannot judge that successor gone.
func TestDoctorProtectsHandoverAfterSnapshot(t *testing.T) {
	for _, successor := range []state.Party{
		claimMe,
		{HostSession: "codex_successor", Name: "Codex successor"},
	} {
		for _, tc := range []struct {
			name        string
			startOffset time.Duration
			wantHeld    bool
		}{
			{"after snapshot", time.Second, true},
			{"before launch", -time.Minute, true},
			{"startup boundary", -replyWait, true},
			{"failed startup expired", -replyWait - time.Second, false},
		} {
			t.Run(successor.Name+"/"+tc.name, func(t *testing.T) {
				a := claimApp(t)
				_, _, stops := lifecycleDocker(t, a)
				holdLease(t, a, labOne, claimOther)
				dir := lease.Dir(a.cfg.LeaseDir)
				before, err := dir.Get(labOne)
				if err != nil || before == nil {
					t.Fatalf("predecessor's lease: %v, %v", before, err)
				}
				// The doctor only saw the predecessor. Both start paths
				// record and transfer under the lock, then launch outside it.
				snapshot := []*claude.Session{{ID: claimOther.Session, Name: claimOther.Name}}
				if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.Starts = append(st.Starts, state.Start{Party: successor, At: a.now.Add(tc.startOffset)})
					_, err := handClaims(st, dir, claimOther, successor)
					return nil, err
				}); err != nil {
					t.Fatal(err)
				}
				lines := a.doctorLabs(context.Background(), snapshot, doctorRun{by: watchParty})
				h, err := dir.Get(labOne)
				if err != nil || (h != nil) != tc.wantHeld {
					t.Fatalf("holder=%+v, error=%v, chores=%v; want held %v", h, err, lines, tc.wantHeld)
				}
				if h != nil && (!h.Party().Is(successor) || h.Since != before.Since) {
					t.Fatalf("inherited claim changed: %+v", h)
				}
				st, err := a.store.Read()
				if err != nil {
					t.Fatal(err)
				}
				if tc.wantHeld && (len(st.LabIdle) != 0 || !st.Released[labOne].IsZero()) || *stops != 0 {
					t.Fatalf("handover lifecycle state: idle=%v, releases=%v, stops=%d, error=%v", st.LabIdle, st.Released, *stops, err)
				}
			})
		}
	}
}
