package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

const claimSupervisor = "Supervisor"

var (
	claimMe    = state.Party{Session: "s-me", Name: "Agent me"}
	claimOther = state.Party{Session: "s-other", Name: "Agent other"}
	claimFirst = state.Party{Session: "s-first", Name: "Agent first"}
	claimSup   = state.Party{Session: "s-sup", Name: claimSupervisor}
)

// claimApp is the session "Agent me" under a supervisor that granted
// grants, in their order; the lab agentlab-1's kind cluster runs. The lease
// store is the test's directory, the kubeconfig kind's stub.
func claimApp(t *testing.T, grants ...state.Grant) *app {
	t.Helper()
	a, _, _ := secretApp(t)
	a.as = ""
	t.Setenv("CLAUDE_CODE_SESSION_ID", claimMe.Session)
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_NAME", claimMe.Name)
	was := plat.Machine
	plat.Machine = tableMachine{plat.Machine}
	t.Cleanup(func() { plat.Machine = was })
	a.kindClusters = func() ([]string, error) { return []string{labCluster}, nil }
	a.cfg.LeaseDir = t.TempDir()
	a.cfg.Resources = []string{labOne, labTwo, graveler}
	a.cfg.Labs = map[string]string{labOne: labCluster, labTwo: labTwo}
	labDirs := labConfigs(t, labCluster, labTwo)
	a.labDirs = func() (map[string]string, error) { return labDirs, nil }
	a.cfg.GrantTTL = config.Duration{Duration: 30 * time.Minute}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.SetSupervisorRole(state.Role{Holder: &state.Supervisor{Party: claimSup}})
		st.Grants = grants
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	return a
}

func claimGrant(res string, to state.Party, ago time.Duration) state.Grant {
	return state.Grant{Resource: res, To: to, At: relayNow.Add(-ago).UTC()}
}

func holdLease(t *testing.T, a *app, res string, p state.Party) {
	t.Helper()
	if _, err := lease.Dir(a.cfg.LeaseDir).Claim(res, lease.Holder{Env: res, Session: p.Session, Name: p.Name, Purpose: "a proof",
		Since: relayNow.Add(-10 * time.Minute).UTC().Format("2006-01-02T15:04:05Z")}); err != nil {
		t.Fatal(err)
	}
}

// A claim whose grant waits says queued first, with its place and whom it
// waits behind, exits 3 and prints no export line: it holds nothing.
func TestLeaseClaimQueued(t *testing.T) {
	a := claimApp(t, claimGrant(labOne, claimMe, 5*time.Minute), claimGrant(labTwo, claimFirst, 5*time.Minute), claimGrant(labTwo, claimMe, 4*time.Minute))
	holdLease(t, a, labOne, claimOther)
	for res, want := range map[string]string{
		labOne: `queued: number 1 behind "Agent other", who holds agentlab-1 since 01:50: a proof;`,
		labTwo: `queued: number 2 behind "Agent first", granted agentlab-2 first`,
	} {
		out, err := runLease(a, "claim", res, "-p", "my proof")
		if Code(err) != ExitRefused || !strings.HasPrefix(out, want) {
			t.Errorf("claim of %s: %q, exit %d; want exit 3 and the first line %q", res, out, Code(err), want)
		}
		if !strings.Contains(out, "; not yours: --park waits in the queue without a process, --wait up to 9m in the foreground\n") {
			t.Errorf("a queued claim of %s names no way to wait: %q", res, out)
		}
		if strings.Contains(out, "KUBECONFIG") || strings.Count(out, "\n") != 1 {
			t.Errorf("a queued claim of %s says more than its line: %q", res, out)
		}
		if h, _ := lease.Dir(a.cfg.LeaseDir).Get(res); h != nil && h.Party().Is(claimMe) {
			t.Errorf("a queued claim of %s took the lease", res)
		}
	}
}

// A claim without a grant says refused first, with the reason, and exits 3.
func TestLeaseClaimRefused(t *testing.T) {
	a := claimApp(t)
	holdLease(t, a, graveler, claimOther)
	for res, want := range map[string]string{
		graveler: `refused: graveler is held by "Agent other" since 01:50: a proof`,
		labTwo:   `refused: agentlab-2: supervisor "Supervisor"`,
	} {
		out, err := runLease(a, "claim", res, "-p", "my proof")
		if Code(err) != ExitRefused || !strings.HasPrefix(out, want) || strings.Contains(out, "KUBECONFIG") {
			t.Errorf("claim of %s: %q, exit %d; want exit 3 and the first line %q", res, out, Code(err), want)
		}
	}
}

// A granted claim says held first and exits 0; a kind lab's export line
// follows it, any other resource has none. A second claim says held too.
func TestLeaseClaimHeld(t *testing.T) {
	a := claimApp(t, claimGrant(labOne, claimMe, time.Minute), claimGrant(graveler, claimMe, time.Minute))
	out, err := runLease(a, "claim", labOne, "-p", "my proof")
	want := "held by you since 02:00: claimed agentlab-1 (kind cluster agentlab, running)\nexport KUBECONFIG=" + labKubeconfig(a.cfg.LeaseDir, labOne) + "\n"
	if err != nil || out != want {
		t.Errorf("claim of the lab: %q, %v; want %q", out, err, want)
	}
	if h, _ := lease.Dir(a.cfg.LeaseDir).Get(labOne); h == nil || !h.Party().Is(claimMe) {
		t.Errorf("the held claim recorded no lease: %+v", h)
	}
	out, err = runLease(a, "claim", labOne, "-p", "my proof")
	if err != nil || !strings.HasPrefix(out, "held by you since 02:00: agentlab-1 (kind cluster agentlab, running) was yours already\nexport KUBECONFIG=") {
		t.Errorf("a second claim of the lab: %q, %v", out, err)
	}
	out, err = runLease(a, "claim", graveler, "-p", "my proof")
	if err != nil || out != "held by you since 02:00: claimed graveler\n" {
		t.Errorf("claim of an installation: %q, %v", out, err)
	}
}

// A claim with --wait claims again until it is held: the holder's release
// lets the queued claim through.
func TestLeaseClaimWaitsUntilHeld(t *testing.T) {
	a := claimApp(t, claimGrant(labOne, claimMe, 5*time.Minute))
	holdLease(t, a, labOne, claimOther)
	var pauses []time.Duration
	was := claimPause
	t.Cleanup(func() { claimPause = was })
	claimPause = func(_ context.Context, d time.Duration) error {
		pauses = append(pauses, d)
		if len(pauses) == 2 {
			return lease.Dir(a.cfg.LeaseDir).Release(labOne)
		}
		return nil
	}
	out, err := runLease(a, "claim", labOne, "-p", "my proof", "--wait", "5m")
	if err != nil || !strings.HasPrefix(out, "held by you since 02:00: claimed agentlab-1") || len(pauses) != 2 {
		t.Errorf("a waiting claim: %q, %v after %v; want held after two pauses", out, err, pauses)
	}
}

// A claim with --wait that is never held says the last outcome and how
// long it waited, and exits 3.
func TestLeaseClaimWaitRunsOut(t *testing.T) {
	a := claimApp(t)
	holdLease(t, a, graveler, claimOther)
	var pauses []time.Duration
	was := claimPause
	t.Cleanup(func() { claimPause = was })
	claimPause = func(_ context.Context, d time.Duration) error { pauses = append(pauses, d); return nil }
	out, err := runLease(a, "claim", graveler, "-p", "my proof", "--wait", "25s")
	want := []time.Duration{claimPoll, claimPoll, 5 * time.Second}
	if Code(err) != ExitRefused || !strings.HasPrefix(out, `refused: graveler is held by "Agent other"`) || !strings.HasSuffix(out, " (waited "+dur(25*time.Second)+")\n") ||
		len(pauses) != len(want) || pauses[2] != want[2] {
		t.Errorf("a claim that waited out: %q, exit %d, pauses %v", out, Code(err), pauses)
	}
}

// claimAgent puts "Agent me" on the roster with a task: it parks.
func claimAgent(t *testing.T, a *app) {
	t.Helper()
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: claimMe, Task: parkTask, AssignedAt: relayNow}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// settleParks runs a watch poll's park steps: it marks the settled parks
// resumable, then resumes them (agents.autoResume).
func settleParks(t *testing.T, a *app) {
	t.Helper()
	a.cfg.Agents.AutoResume = true
	w := &watcher{app: a, last: map[string]time.Time{}, reported: map[string]bool{}}
	for range 2 {
		st, err := a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		w.autoResume(context.Background(), st)
		parks := w.settledParks(context.Background(), st)
		if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			_, evs := markResumable(st, parks, true, a.now)
			return evs, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// A queued claim with --park says its queued line first, unchanged, parks
// the agent on the lease and exits 3; no process waits. The watch leaves it
// parked while the lease is held and wakes it once, with the words to claim
// it, when the holder released it and its grant is first.
func TestLeaseClaimParkWakesOnGrant(t *testing.T) {
	a := claimApp(t, claimGrant(labOne, claimMe, 5*time.Minute))
	claimAgent(t, a)
	woke := stubWakes(t)
	holdLease(t, a, labOne, claimOther)
	out, err := runLease(a, "claim", labOne, "-p", "my proof", "--park")
	first, park, _ := strings.Cut(out, "\n")
	if Code(err) != ExitRefused || !strings.HasPrefix(first, `queued: number 1 behind "Agent other"`) {
		t.Fatalf("a parked claim: %q, exit %d; want exit 3 and the queued line first", out, Code(err))
	}
	if !strings.HasPrefix(park, "Agent me: parked on lease agentlab-1: the lease agentlab-1: my proof;") || !strings.Contains(park, "End your turn now.") {
		t.Errorf("the park line: %q", park)
	}
	settleParks(t, a)
	if len(*woke) > 0 {
		t.Fatalf("woken while the lease is held: %q", *woke)
	}
	if err := lease.Dir(a.cfg.LeaseDir).Release(labOne); err != nil {
		t.Fatal(err)
	}
	settleParks(t, a)
	settleParks(t, a)
	want := watchParty.Name + " → " + claimMe.Name + ": Your park on lease agentlab-1 (the lease agentlab-1: my proof) is over: agentlab-1 is free and yours: " +
		"claim it now with `beekeeper lease claim agentlab-1 -p \"<purpose>\"`, within 30m. Re-query the live state of your task and continue it."
	if !slices.Equal(*woke, []string{want}) {
		t.Errorf("woke %q\nwant %q", *woke, want)
	}
	if ag := agentOf(t, &watcher{app: a}); ag.Park != nil || ag.Task != parkTask {
		t.Errorf("after the wake: task %q park %+v", ag.Task, ag.Park)
	}
	if out, err := runLease(a, "claim", labOne, "-p", "my proof"); err != nil || !strings.HasPrefix(out, "held by you since") {
		t.Errorf("the claim after the wake: %q, %v", out, err)
	}
}

// A lease granted to another session first does not wake a parked claim:
// the lease freed, it stays parked behind that grant.
func TestLeaseParkStaysBehindAnotherGrant(t *testing.T) {
	a := claimApp(t, claimGrant(labTwo, claimFirst, 5*time.Minute), claimGrant(labTwo, claimMe, 4*time.Minute))
	claimAgent(t, a)
	woke := stubWakes(t)
	holdLease(t, a, labTwo, claimOther)
	if out, err := runLease(a, "claim", labTwo, "-p", "my proof", "--park"); Code(err) != ExitRefused || !strings.Contains(out, "parked on lease agentlab-2") {
		t.Fatalf("a parked claim: %q, exit %d", out, Code(err))
	}
	if err := lease.Dir(a.cfg.LeaseDir).Release(labTwo); err != nil {
		t.Fatal(err)
	}
	settleParks(t, a)
	if len(*woke) > 0 {
		t.Errorf("woken while %q is granted the lease first: %q", claimFirst.Name, *woke)
	}
	if ag := agentOf(t, &watcher{app: a}); ag.Park == nil || ag.Park.On != "lease agentlab-2" || !ag.Park.Resumable.IsZero() {
		t.Errorf("the park after the release: %+v", ag.Park)
	}
}

// A refused claim with --park parks nothing: no grant waits for it.
func TestLeaseClaimParkRefusedParksNothing(t *testing.T) {
	a := claimApp(t)
	claimAgent(t, a)
	holdLease(t, a, graveler, claimOther)
	out, err := runLease(a, "claim", graveler, "-p", "my proof", "--park")
	if Code(err) != ExitRefused || !strings.HasPrefix(out, "refused: graveler is held") || !strings.Contains(out, "\nnot parked: no grant of graveler to you waits") {
		t.Errorf("a refused claim with --park: %q, exit %d", out, Code(err))
	}
	if ag := agentOf(t, &watcher{app: a}); ag.Park != nil {
		t.Errorf("parked on a refused claim: %+v", ag.Park)
	}
}

// --wait is a foreground wait of at most claimWaitMax; a longer one is
// refused, naming --park.
func TestLeaseClaimWaitIsShort(t *testing.T) {
	a := claimApp(t, claimGrant(labOne, claimMe, 5*time.Minute))
	_, err := runLease(a, "claim", labOne, "-p", "my proof", "--wait", "4h")
	if Code(err) != ExitUsage || !strings.Contains(err.Error(), "--park") {
		t.Errorf("--wait 4h: %v, exit %d; want a usage error naming --park", err, Code(err))
	}
}

// asSupervisor makes the calling session the supervisor of claimApp.
func asSupervisor(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_ID", claimSup.Session)
	t.Setenv("CLAUDE_CODE_SESSION_NAME", claimSup.Name)
}

// A worker's grant --first is refused; the supervisor's grant --first, and
// its `yours`, to a session queued second or third moves its grant to the head of the queue: the grants it passes
// keep their order behind it, and its claim is held.
func TestGrantFirstMovesTheGrantToTheHead(t *testing.T) {
	a := claimApp(t, claimGrant(labTwo, claimFirst, 5*time.Minute), claimGrant(labTwo, claimOther, 4*time.Minute), claimGrant(labTwo, claimMe, 3*time.Minute))
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		for _, p := range []state.Party{claimFirst, claimOther, claimMe} {
			st.Agents = append(st.Agents, state.Agent{Party: p, Registered: relayNow})
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	queue := func() []string {
		t.Helper()
		st, err := a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, g := range lease.Pending(st, labTwo, false, a.now, a.cfg.GrantTTL.Duration) {
			names = append(names, g.To.Name)
		}
		return names
	}
	out, err := runLease(a, "grant", labTwo, claimOther.Name, "--first")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), `only the supervisor "Supervisor" puts a grant ahead of the queue`) {
		t.Errorf("a worker's grant --first: %q, %v; want refused", out, err)
	}
	if got, want := queue(), []string{claimFirst.Name, claimOther.Name, claimMe.Name}; !slices.Equal(got, want) {
		t.Errorf("after the worker's refused grant --first: %q, want %q", got, want)
	}
	asSupervisor(t)
	out, err = runLease(a, "grant", labTwo, claimOther.Name, "--first")
	if err != nil || !strings.HasPrefix(out, `agentlab-2 is granted to "Agent other" first now, ahead of "Agent first": it claims with`) {
		t.Errorf("grant --first to the second: %q, %v", out, err)
	}
	if got, want := queue(), []string{claimOther.Name, claimFirst.Name, claimMe.Name}; !slices.Equal(got, want) {
		t.Errorf("after the grant to the second: %q, want %q", got, want)
	}
	got := a.yoursGrants(nil, claimSup, claimMe.Name, []string{labTwo})
	if !strings.Contains(got, `agentlab-2 is granted to "Agent me" first now, ahead of "Agent other", then "Agent first"`) {
		t.Errorf("yours to the third: %q", got)
	}
	if got, want := queue(), []string{claimMe.Name, claimOther.Name, claimFirst.Name}; !slices.Equal(got, want) {
		t.Errorf("after the yours to the third: %q, want %q", got, want)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", claimMe.Session)
	t.Setenv("CLAUDE_CODE_SESSION_NAME", claimMe.Name)
	if out, err := runLease(a, "claim", labTwo, "-p", "my proof"); err != nil || !strings.HasPrefix(out, "held by you since") {
		t.Errorf("the claim of the moved grant: %q, %v", out, err)
	}
	if got, want := queue(), []string{claimOther.Name, claimFirst.Name}; !slices.Equal(got, want) {
		t.Errorf("after the claim: %q, want %q", got, want)
	}
}

// The supervisor's new grant --first goes ahead of the earlier grants.
func TestNewGrantFirst(t *testing.T) {
	a := claimApp(t, claimGrant(labTwo, claimFirst, 5*time.Minute))
	holdLease(t, a, labTwo, claimOther)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{Party: claimMe, Registered: relayNow})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	asSupervisor(t)
	out, err := runLease(a, "grant", labTwo, claimMe.Name, "--first")
	if err != nil || !strings.HasPrefix(out, `granted agentlab-2 to "Agent me", first in its queue ahead of "Agent first": "Agent other" holds it (a proof)`) {
		t.Errorf("a new grant --first: %q, %v", out, err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if q := lease.Pending(st, labTwo, true, a.now, a.cfg.GrantTTL.Duration); len(q) != 2 || !q[0].To.Is(claimMe) || !q[1].To.Is(claimFirst) {
		t.Errorf("queue %+v", q)
	}
}

// A grant --first under --as, a person's name, is refused too while a
// supervisor is recorded: --as names no session the role could match.
func TestGrantFirstAsAPersonIsRefused(t *testing.T) {
	a := claimApp(t, claimGrant(labTwo, claimFirst, 5*time.Minute))
	a.as = personTimo
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{Party: claimMe, Registered: relayNow})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if out, err := runLease(a, "grant", labTwo, claimMe.Name, "--first"); Code(err) != ExitRefused {
		t.Errorf("grant --first --as a person: %q, %v; want refused", out, err)
	}
}
