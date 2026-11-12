package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
)

func TestHostCapacityReasons(t *testing.T) {
	for _, tc := range []struct {
		out  string
		code int
		err  error
		want string
	}{
		{loopRoomJSON, 0, nil, ""},
		{`{"room":0,"blocks":["host maxBusy reached"]}`, 0, nil, "host maxBusy reached"},
		{`{"room":0,"verdict":"no start: provider quota"}`, 0, nil, "provider quota"},
		{"malformed capacity output", 0, nil, "answer is unreadable"},
		{"{}", 0, nil, "capacity answer is"},
		{"policy mismatch", 3, nil, "policy mismatch"},
		{"", 0, errors.New("host unreachable"), "host unreachable"},
	} {
		got := loop.HostCapacityReason(tc.out, tc.code, tc.err)
		if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("capacity %q: %q, want %q", tc.out, got, tc.want)
		}
	}
}

func TestProbationKeepsCompatibleHostAdmission(t *testing.T) {
	a, _ := sshStartTestApp(t)
	withCoordinatorVersion(t, hostNewRelease)
	previous := coordinatorJournal
	coordinatorJournal = func() (*update.Journal, error) {
		return &update.Journal{Probation: coordinatorProbationPending, Candidate: update.Binary{Version: hostNewRelease}}, nil
	}
	t.Cleanup(func() { coordinatorJournal = previous })
	h := a.cfg.Agents.Hosts[sshTestSelector]
	probe := a.executionProbe
	a.executionProbe = func(ctx context.Context, host execution.Host) (execution.Capabilities, error) {
		caps, err := probe(ctx, host)
		caps.Version = hostOldRelease
		return caps, err
	}
	for _, legacy := range []string{"", "looking up release candidate: GitHub rate limit exceeded", update.ErrProbationWatched.Error()} {
		if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			st.HostPins = []state.HostPin{{Selector: sshTestSelector, Identity: h.Identity, Version: hostOldRelease, PolicyDigest: h.PolicyDigest, Held: legacy}}
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := a.updateExecutionHost(t.Context(), sshTestSelector); err == nil || !strings.Contains(err.Error(), hostUpdateDeferred) {
			t.Fatalf("probation must still defer updates: %v", err)
		}
		if err := a.admitExecution(sshTestSelector, h); err != nil {
			t.Fatalf("probation blocked host admission: %v", err)
		}
		if _, err := a.probeExecution(t.Context(), h); err != nil {
			t.Fatalf("compatible host failed probe: %v", err)
		}
	}
}

func TestLookupFailurePreservesExistingInstallationHold(t *testing.T) {
	a, _ := sshStartTestApp(t)
	withCoordinatorVersion(t, hostNewRelease)
	stubHostUpdates(t, a, nil)
	lookupRelease = func(context.Context, string) (update.Release, error) {
		return update.Release{}, errors.New("lookup unavailable")
	}
	h := a.cfg.Agents.Hosts[sshTestSelector]
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.HostPins = []state.HostPin{{Selector: sshTestSelector, Identity: h.Identity, Held: "policy preparation failed"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.updateExecutionHost(t.Context(), sshTestSelector); err == nil {
		t.Fatal("lookup succeeded")
	}
	if err := a.admitExecution(sshTestSelector, h); err == nil {
		t.Fatal("lookup failure lifted installation hold")
	}
}

func TestHostPlacementDiagnosticsSurviveLoopRestart(t *testing.T) {
	r := newLoopRig(t)
	r.sshLoopHost(t, true)
	l := &loop.Loop{Host: r.a, Said: map[string]bool{}}
	l.RecordHostPlacement(sshTestSelector, sshTestRepository, "repository is not mapped")
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	views := r.a.hostPlacementViews(st)
	if len(views) != 1 || views[0].At.IsZero() || !strings.Contains(views[0].Line(), "not mapped") {
		t.Fatalf("missing dated placement: %+v", views)
	}
	if err := r.a.supervisorStatus(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.out.String(), views[0].Line()) {
		t.Fatalf("status lost host reason: %s", r.out)
	}
	encoded, err := json.Marshal(&capacity.View{Hosts: views})
	if err != nil || !strings.Contains(string(encoded), `"selector":"`+sshTestSelector+`"`) || !strings.Contains(string(encoded), `"reason":"repository is not mapped"`) {
		t.Fatalf("capacity JSON lost host diagnostics: %s, %v", encoded, err)
	}
}

func TestHostStarvationSurvivesRestartsAndClearsOnRemoteStart(t *testing.T) {
	r := newLoopRig(t)
	r.sshLoopHost(t, true)
	start := r.a.now
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = []state.Start{{At: start, Kind: workerRole}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		st, err := r.a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		// New loop object on every observation proves the alarm is durable.
		if err := (&loop.Loop{Host: r.a}).CheckHostUtilization(st); err != nil {
			t.Fatal(err)
		}
	}
	check()
	r.a.now = start.Add(time.Hour - time.Second)
	check()
	if n := len(r.logged(t, "supervisor.file")); n != 0 {
		t.Fatalf("early alarm: %d", n)
	}
	r.a.now = start.Add(time.Hour)
	check()
	check()
	if n := len(r.logged(t, "supervisor.file")); n != 1 {
		t.Fatalf("want one standing alarm, got %d", n)
	}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = append(st.Starts, state.Start{At: r.a.now, Execution: &state.ExecutionRecord{Selector: sshTestSelector, Phase: execution.Running}})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	check()
	st, _ := r.a.store.Read()
	if !st.HostIdleSince.IsZero() || st.HostIdleReported {
		t.Fatal("remote start left starvation active")
	}
	r.a.now = r.a.now.Add(time.Second)
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = append(st.Starts, state.Start{At: r.a.now, Kind: workerRole})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	check()
	r.a.now = r.a.now.Add(time.Hour)
	check()
	if n := len(r.logged(t, "supervisor.file")); n != 2 {
		t.Fatalf("new starvation episode: %d alarms", n)
	}
}

func TestIdleDeskAndUnsentRemoteReservationDoNotHideStarvation(t *testing.T) {
	r := newLoopRig(t)
	r.sshLoopHost(t, true)
	l := &loop.Loop{Host: r.a}
	if err := l.CheckHostUtilization(&state.State{}); err != nil {
		t.Fatal(err)
	}
	if n := len(r.logged(t, "supervisor.file")); n != 0 {
		t.Fatal("idle desk reported starvation")
	}
	st := &state.State{Starts: []state.Start{
		{At: r.a.now.Add(-2 * time.Hour), Kind: workerRole},
		{At: r.a.now, Execution: &state.ExecutionRecord{Phase: executionPending, StartUnsent: true}},
	}}
	if err := l.CheckHostUtilization(st); err != nil {
		t.Fatal(err)
	}
	if n := len(r.logged(t, "supervisor.file")); n != 1 {
		t.Fatal("unsent reservation hid starvation")
	}
}
