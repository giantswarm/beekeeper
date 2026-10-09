package cmd

import (
	"context"
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
	a.cfg.GrantTTL = config.Duration{Duration: 30 * time.Minute}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.SetSupervisorRole(state.Role{Holder: &state.Supervisor{Party: state.Party{Session: "s-sup", Name: claimSupervisor}}})
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
		if !strings.Contains(out, "; not yours: park on `beekeeper lease status "+res+"` in a probe timer, or claim with --wait <duration>\n") {
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
