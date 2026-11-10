package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

const claimSupervisor = "Supervisor"

var (
	claimMe    = state.Party{Session: "s-me", Name: "Agent me"}
	claimOther = state.Party{Session: "s-other", Name: "Agent other"}
	claimFirst = state.Party{Session: "s-first", Name: "Agent first"}
)

// claimApp is the session "Agent me" with grants queued, in their order; the lab agentlab-1's kind cluster runs. The lease
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
	a.labDocker = func(context.Context, ...string) ([]byte, error) { return nil, nil }
	a.cfg.Resources = []string{labOne, labTwo, graveler}
	a.cfg.Labs = map[string]string{labOne: labCluster, labTwo: labTwo}
	labDirs := labConfigs(t, labCluster, labTwo)
	a.labDirs = func() (map[string]string, error) { return labDirs, nil }
	a.cfg.GrantTTL = config.Duration{Duration: 30 * time.Minute}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
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
	holdStartingLease(t, a, labOne, claimOther)
	for res, want := range map[string]string{
		labOne: `queued: number 1 behind "Agent other", who holds agentlab-1 since 01:50: a proof;`,
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
	holdStartingLease(t, a, graveler, claimOther)
	for res, want := range map[string]string{
		graveler: `refused: graveler is held by "Agent other" since 01:50: a proof`,
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
	holdStartingLease(t, a, labOne, claimOther)
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
	holdStartingLease(t, a, graveler, claimOther)
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
	holdStartingLease(t, a, labOne, claimOther)
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
	assertFreshQueue(t, &watcher{app: a}, *woke, "free and yours")

	if out, err := runLease(a, "claim", labOne, "-p", "my proof"); err != nil || !strings.HasPrefix(out, "held by you since") {
		t.Errorf("the claim after the wake: %q, %v", out, err)
	}
}

// A refused claim with --park parks nothing: no grant waits for it.
func TestLeaseClaimParkRefusedParksNothing(t *testing.T) {
	a := claimApp(t)
	claimAgent(t, a)
	holdStartingLease(t, a, graveler, claimOther)
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

// asSupervisor makes a's caller the supervisor loop.
func asSupervisor(a *app) { a.as = loopParty.Name }

// A worker's grant --first is refused; the supervisor loop's grant --first
// to a session queued second or third moves its grant to the head of the
// queue: the grants it passes keep their order behind it, and its claim is
// held.
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
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), `only the supervisor loop or a person puts a grant ahead of the queue`) {
		t.Errorf("a worker's grant --first: %q, %v; want refused", out, err)
	}
	if got, want := queue(), []string{claimFirst.Name, claimOther.Name, claimMe.Name}; !slices.Equal(got, want) {
		t.Errorf("after the worker's refused grant --first: %q, want %q", got, want)
	}
	asSupervisor(a)
	out, err = runLease(a, "grant", labTwo, claimOther.Name, "--first")
	if err != nil || !strings.HasPrefix(out, `agentlab-2 is granted to "Agent other" first now, ahead of "Agent first": it claims with`) {
		t.Errorf("grant --first to the second: %q, %v", out, err)
	}
	if got, want := queue(), []string{claimOther.Name, claimFirst.Name, claimMe.Name}; !slices.Equal(got, want) {
		t.Errorf("after the grant to the second: %q, want %q", got, want)
	}
	out, err = runLease(a, "grant", labTwo, claimMe.Name, "--first")
	if err != nil || !strings.HasPrefix(out, `agentlab-2 is granted to "Agent me" first now, ahead of "Agent other", then "Agent first"`) {
		t.Errorf("grant --first to the third: %q, %v", out, err)
	}
	if got, want := queue(), []string{claimMe.Name, claimOther.Name, claimFirst.Name}; !slices.Equal(got, want) {
		t.Errorf("after the grant to the third: %q, want %q", got, want)
	}
	a.as = ""
	t.Setenv("CLAUDE_CODE_SESSION_ID", claimMe.Session)
	t.Setenv("CLAUDE_CODE_SESSION_NAME", claimMe.Name)
	if out, err := runLease(a, "claim", labTwo, "-p", "my proof"); err != nil || !strings.HasPrefix(out, "held by you since") {
		t.Errorf("the claim of the moved grant: %q, %v", out, err)
	}
	if got, want := queue(), []string{claimOther.Name, claimFirst.Name}; !slices.Equal(got, want) {
		t.Errorf("after the claim: %q, want %q", got, want)
	}
}

// The supervisor loop's new grant --first goes ahead of the earlier grants.
func TestNewGrantFirst(t *testing.T) {
	a := claimApp(t, claimGrant(labTwo, claimFirst, 5*time.Minute))
	holdStartingLease(t, a, labTwo, claimOther)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{Party: claimMe, Registered: relayNow})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	asSupervisor(a)
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

func TestLeaseClaimEndedPredecessor(t *testing.T) {
	a := claimApp(t)
	predecessor := state.Party{Session: "old-session", Name: claimMe.Name}
	holdLease(t, a, graveler, predecessor)
	out, err := runLease(a, "claim", graveler, "-p", "successor work")
	if err != nil || !strings.HasPrefix(out, "held by you") {
		t.Fatalf("claim: %q, %v", out, err)
	}
	h, err := lease.Dir(a.cfg.LeaseDir).Get(graveler)
	if err != nil || h == nil || !h.Party().Is(claimMe) {
		t.Fatalf("holder: %+v, %v", h, err)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == leaseReleaseGoneVerb })
	if err != nil || len(evs) != 1 {
		t.Fatalf("stale release log: %+v, %v", evs, err)
	}
}

func TestRegisterSuccessorInheritsClaims(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprint("managed=", started), func(t *testing.T) {
			a := claimApp(t)
			old := state.Party{Session: "predecessor", Name: claimMe.Name}
			holdLease(t, a, graveler, old)
			before, _ := lease.Dir(a.cfg.LeaseDir).Get(graveler)
			err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents = []state.Agent{{Party: old}}
				st.Grants = []state.Grant{claimGrant(labOne, old, time.Minute)}
				live := func(state.Party) bool { return false }
				if started {
					_, err := a.recordStart(st, state.Start{Party: claimMe, At: a.now}, "", "", live)
					return nil, err
				}
				_, err := a.registerAgent(st, claimMe, live, a.now)
				return nil, err
			})
			if err != nil {
				t.Fatal(err)
			}
			h, err := lease.Dir(a.cfg.LeaseDir).Get(graveler)
			if err != nil || h == nil || !h.Party().Is(claimMe) || h.Since != before.Since || h.Purpose != before.Purpose {
				t.Fatalf("inherited holder: %+v, %v", h, err)
			}
			st, _ := a.store.Read()
			if len(st.Grants) != 1 || !st.Grants[0].To.Is(claimMe) {
				t.Fatalf("grants: %+v", st.Grants)
			}
			out, err := runLease(a, "claim", graveler, "-p", "successor work")
			if err != nil || !strings.Contains(out, "was yours already") {
				t.Fatalf("inherited claim: %q, %v", out, err)
			}
		})
	}
}

func TestLeaseQueueAndProofHandoff(t *testing.T) {
	a := claimApp(t, claimGrant(graveler, claimOther, 2*time.Minute), claimGrant(graveler, claimMe, time.Minute))
	out, err := runLease(a, "claim", graveler, "-p", "inspect")
	if Code(err) != ExitRefused || !strings.Contains(out, "number 2") || !strings.Contains(out, claimOther.Name) {
		t.Fatalf("second bypasses free queue: %q, %v", out, err)
	}
	// The head claims a destructive proof and holds the resource.
	t.Setenv("CLAUDE_CODE_SESSION_ID", claimOther.Session)
	t.Setenv("CLAUDE_CODE_SESSION_NAME", claimOther.Name)
	out, err = runLease(a, "claim", graveler, "-p", "node-restart proof")
	if err != nil {
		t.Fatalf("head claim: %q, %v", out, err)
	}
	h, err := lease.Dir(a.cfg.LeaseDir).Get(graveler)
	if err != nil || h == nil || !h.DestructiveProof {
		t.Fatalf("proof not recorded: %+v, %v", h, err)
	}
	// The holder is alive: a competing claim must queue, not release it.
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = append(st.Starts, state.Start{Party: claimOther, At: a.now})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", claimMe.Session)
	t.Setenv("CLAUDE_CODE_SESSION_NAME", claimMe.Name)
	out, err = runLease(a, "claim", graveler, "-p", "inspect")
	if Code(err) != ExitRefused || !strings.Contains(out, claimOther.Name) || !strings.Contains(out, "number 1") {
		t.Fatalf("live lease bypassed: %q, %v", out, err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", claimOther.Session)
	t.Setenv("CLAUDE_CODE_SESSION_NAME", claimOther.Name)
	if out, err = runLease(a, "release", graveler); err != nil {
		t.Fatalf("release: %q, %v", out, err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", claimMe.Session)
	t.Setenv("CLAUDE_CODE_SESSION_NAME", claimMe.Name)
	out, err = runLease(a, "claim", graveler, "-p", "inspect")
	if err != nil || !strings.Contains(out, "previous destructive proof") || !strings.Contains(out, "node-restart proof") {
		t.Fatalf("handoff: %q, %v", out, err)
	}
}

func TestLeaseGrantHeldQueuePosition(t *testing.T) {
	a := claimApp(t, claimGrant(graveler, claimFirst, time.Minute))
	holdLease(t, a, graveler, claimOther)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{Party: claimMe, Registered: relayNow})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	out, err := runLease(a, "grant", graveler, claimMe.Name)
	if err != nil || !strings.Contains(out, "number 2 in its queue") || !strings.Contains(out, claimOther.Name) {
		t.Fatalf("held grant: %q, %v", out, err)
	}
}

// Timer commands must not acquire (or queue) a lease for the watching session.
func TestLeaseClaimRefusesTimerCommands(t *testing.T) {
	for _, kind := range []string{"probe", "run"} {
		t.Run(kind, func(t *testing.T) {
			a := claimApp(t)
			t.Setenv(envTimerCommand, kind)
			_, err := runLease(a, "claim", labOne, "-p", "timer work")
			if Code(err) != ExitRefused || !strings.Contains(err.Error(), "watch session, not the timer setter") {
				t.Fatalf("claim: %v; want refusal explaining the inherited session", err)
			}
			if h, err := lease.Dir(a.cfg.LeaseDir).Get(labOne); err != nil || h != nil {
				t.Fatalf("refused claim changed lease: %+v, %v", h, err)
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Grants) != 0 {
				t.Fatalf("refused claim queued grants: %+v", st.Grants)
			}
		})
	}
}

// A holder still in its startup window must keep competing claims queued.
func holdStartingLease(t *testing.T, a *app, res string, p state.Party) {
	t.Helper()
	holdLease(t, a, res, p)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = append(st.Starts, state.Start{Party: p, At: a.now})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRoleRelayTransfersBrowserLease(t *testing.T) {
	a := claimApp(t)
	a.cfg.Resources = append(a.cfg.Resources, browser)
	old := state.Party{Session: "previous-role", Name: "Guide run 1"}
	holdLease(t, a, browser, old)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = append(st.Starts, state.Start{Party: claimMe, Role: guideName, Mode: state.ModeBypass, At: a.now})
		st.Guide = &state.Role{
			Holder: &state.Supervisor{Party: old, Since: a.now.Add(-time.Hour)},
			Relay:  &state.Relay{From: old, To: claimMe, At: a.now.Add(-time.Minute), Expires: a.now.Add(time.Minute)},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.runStart(context.Background(), guideRole, false); err != nil {
		t.Fatal(err)
	}
	h, err := lease.Dir(a.cfg.LeaseDir).Get(browser)
	if err != nil || h == nil || !h.Party().Is(claimMe) {
		t.Fatalf("relayed browser: %+v, %v", h, err)
	}
	out, err := runLease(a, "claim", browser, "-p", "continued role work")
	if err != nil || !strings.Contains(out, "was yours already") {
		t.Fatalf("relayed claim: %q, %v", out, err)
	}
}

// holderStartingKind is a holder still in its startup window.
const holderStartingKind = "starting"

func TestClaimKeepsLiveAndStartingHolders(t *testing.T) {
	for _, kind := range []string{testLiveMode, holderStartingKind, "registered after snapshot", "person", "remote host", "new claim"} {
		t.Run(kind, func(t *testing.T) {
			a := claimApp(t)
			holdLease(t, a, graveler, claimOther)
			dir := lease.Dir(a.cfg.LeaseDir)
			h, _ := dir.Get(graveler)
			var sessions []*claude.Session
			switch kind {
			case testLiveMode:
				sessions = []*claude.Session{{ID: claimOther.Session, Name: claimOther.Name}}
			case holderStartingKind, "registered after snapshot":
				if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
					if kind == holderStartingKind {
						st.Starts = []state.Start{{Party: claimOther, At: a.now}}
					} else {
						st.Agents = []state.Agent{{Party: claimOther, Registered: a.now.Add(time.Second)}}
					}
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			case "person":
				h.Session = ""
			case "remote host":
				h.Host = "remote-fixture"
			case "new claim":
				h.Since = a.now.Format(time.RFC3339)
			}
			if err := dir.Hand(*h); err != nil {
				t.Fatal(err)
			}
			if err := a.releaseEndedClaim(graveler, claimMe, sessions); err != nil {
				t.Fatal(err)
			}
			after, err := dir.Get(graveler)
			if err != nil || after == nil || after.Session != h.Session {
				t.Fatalf("protected holder released: %+v, %v", after, err)
			}
			evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == leaseReleaseGoneVerb })
			if err != nil || len(evs) != 0 {
				t.Fatalf("unexpected release event: %+v, %v", evs, err)
			}
		})
	}
}

func TestLeaseConfiguredInstallation(t *testing.T) {
	const installation = "customer"
	a := claimApp(t)
	a.cfg.Alerts.Installations = []config.Installation{{Name: installation}}
	out, err := runLease(a, "claim", installation, "-p", "cut-over")
	if err != nil || !strings.HasPrefix(out, "held by you since ") {
		t.Fatalf("claim: %q, %v", out, err)
	}
	h, err := lease.Dir(a.cfg.LeaseDir).Get(installation)
	if err != nil || h == nil || !h.Party().Is(claimMe) || h.Purpose != "cut-over" {
		t.Fatalf("holder = %+v, %v", h, err)
	}
	out, err = runLease(a, "status", installation)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), claimMe.Name) || !strings.Contains(err.Error(), "cut-over") {
		t.Fatalf("status: %q, %v", out, err)
	}
	out, err = runLease(a, "list")
	if err != nil || !strings.Contains(out, installation) || !strings.Contains(out, "cut-over") || !strings.Contains(out, claimMe.Name) {
		t.Fatalf("list: %q, %v", out, err)
	}
	if out, err = runLease(a, leaseReleaseVerb, installation); err != nil {
		t.Fatalf("release: %q, %v", out, err)
	}
	if out, err = runLease(a, "status", installation); err != nil || out != "free\n" {
		t.Fatalf("released status: %q, %v", out, err)
	}
	for _, args := range [][]string{{leaseClaimVerb, kindUnknown, "-p", "cut-over"}, {leaseReleaseVerb, kindUnknown}} {
		if _, err := runLease(a, args...); Code(err) != ExitUsage || !strings.Contains(err.Error(), "unknown resource") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestLeaseConfiguredInstallationQueued(t *testing.T) {
	const installation = "customer"
	a := claimApp(t, claimGrant(installation, claimMe, time.Minute))
	a.cfg.Alerts.Installations = []config.Installation{{Name: installation}}
	holdStartingLease(t, a, installation, claimOther)
	out, err := runLease(a, "claim", installation, "-p", "cut-over")
	if Code(err) != ExitRefused || !strings.HasPrefix(out, "queued: number 1 behind ") {
		t.Fatalf("queued claim: %q, %v", out, err)
	}
	if err := lease.Dir(a.cfg.LeaseDir).Release(installation); err != nil {
		t.Fatal(err)
	}
	out, err = runLease(a, "claim", installation, "-p", "cut-over")
	if err != nil || !strings.HasPrefix(out, "held by you since ") {
		t.Fatalf("claim after release: %q, %v", out, err)
	}
}

const (
	leaseClaimVerb   = "claim"
	leaseReleaseVerb = "release"
)
