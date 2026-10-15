package cmd

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

// grantees lists to whom res is granted, in order.
func grantees(t *testing.T, a *app, res string) []string {
	t.Helper()
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, g := range st.Grants {
		if g.Resource == res {
			out = append(out, g.To.Name)
		}
	}
	return out
}

// A grantee queued behind the holder releases its own queued grant: the
// grant leaves the queue, its park on the lease ends, the next grant moves
// up, lease list shows it and the log records it. The holder keeps the lease.
func TestLeaseReleaseWithdrawsTheCallersQueuedGrant(t *testing.T) {
	a := claimApp(t, claimGrant(labOne, claimMe, 5*time.Minute), claimGrant(labOne, claimFirst, 4*time.Minute))
	claimAgent(t, a)
	stubWakes(t)
	holdLease(t, a, labOne, claimOther)
	if out, err := runLease(a, "claim", labOne, "-p", "my proof", "--park"); Code(err) != ExitRefused || !strings.Contains(out, "parked on lease agentlab-1") {
		t.Fatalf("a parked claim: %q, exit %d", out, Code(err))
	}
	out, err := runLease(a, "release", labOne)
	want := `withdrew your grant of agentlab-1 (number 1 in its queue) and the park on it; "Agent first" is first in its queue now` + "\n"
	if err != nil || out != want {
		t.Errorf("release of a queued grant: %q, %v\nwant %q", out, err, want)
	}
	if got := grantees(t, a, labOne); !slices.Equal(got, []string{claimFirst.Name}) {
		t.Errorf("the queue after the withdrawal: %q", got)
	}
	if ag := agentOf(t, &watcher{app: a}); ag.Park != nil || ag.Task != parkTask {
		t.Errorf("after the withdrawal: task %q park %+v", ag.Task, ag.Park)
	}
	if h, _ := lease.Dir(a.cfg.LeaseDir).Get(labOne); h == nil || !h.Party().Is(claimOther) {
		t.Errorf("the withdrawal touched the holder's lease: %+v", h)
	}
	if out, err := runLease(a, "list"); err != nil || !strings.Contains(out, `granted agentlab-1: "Agent first"`) || strings.Contains(out, claimMe.Name) {
		t.Errorf("lease list after the withdrawal: %q, %v", out, err)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == verbLeaseWithdraw })
	if err != nil || len(evs) != 1 || !evs[0].By.Is(claimMe) || evs[0].Detail != `agentlab-1 (number 1 in its queue) and the park on it; "Agent first" is first in its queue now` {
		t.Errorf("the withdrawal's events: %+v, %v", evs, err)
	}
}

// A grant of a free lease is withdrawn the same way; with nothing queued
// after it, the line says so.
func TestLeaseReleaseWithdrawsAGrantOfAFreeLease(t *testing.T) {
	a := claimApp(t, claimGrant(labTwo, claimMe, time.Minute))
	out, err := runLease(a, "release", labTwo)
	if want := "withdrew your grant of agentlab-2 (number 1 in its queue); nothing is queued for it now\n"; err != nil || out != want {
		t.Errorf("release of a free lease's grant: %q, %v; want %q", out, err, want)
	}
	if got := grantees(t, a, labTwo); len(got) != 0 {
		t.Errorf("the queue after the withdrawal: %q", got)
	}
}

// A release of another holder's lease without a grant of it is refused, the
// refusal says no grant waits, and another session's grant stays queued.
func TestLeaseReleaseWithoutAGrantIsRefused(t *testing.T) {
	a := claimApp(t, claimGrant(labOne, claimFirst, time.Minute))
	holdLease(t, a, labOne, claimOther)
	_, err := runLease(a, "release", labOne)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), `held by "Agent other", not by you, and no grant of it to you waits`) {
		t.Errorf("release without a grant: %v, exit %d", err, Code(err))
	}
	if got := grantees(t, a, labOne); !slices.Equal(got, []string{claimFirst.Name}) {
		t.Errorf("a refused release changed the queue: %q", got)
	}
}

// A session revokes only its own grant; another session's grant is the
// supervisor's to revoke, and stays queued when the session tries.
func TestLeaseRevokeOfAnotherSessionsGrantIsRefused(t *testing.T) {
	a := claimApp(t, claimGrant(labOne, claimFirst, 2*time.Minute), claimGrant(labOne, claimMe, time.Minute))
	_, err := runLease(a, "revoke", labOne, "first")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), `granted to "Agent first", not to you: only the supervisor "Supervisor" revokes`) {
		t.Errorf("revoke of another session's grant: %v, exit %d", err, Code(err))
	}
	out, err := runLease(a, "revoke", labOne, "me")
	if err != nil || out != "revoked 1 grant(s) of agentlab-1\n" {
		t.Errorf("revoke of the caller's own grant: %q, %v", out, err)
	}
	if got := grantees(t, a, labOne); !slices.Equal(got, []string{claimFirst.Name}) {
		t.Errorf("the queue after the revokes: %q", got)
	}
}

// The supervisor revokes a parked grantee's grant while another session
// holds the lease: the watch resumes the grantee at once with the words to
// ask again, instead of leaving it parked until the lease is free.
func TestLeaseRevokeResumesTheParkedGrantee(t *testing.T) {
	a := claimApp(t, claimGrant(labOne, claimMe, 5*time.Minute))
	claimAgent(t, a)
	woke := stubWakes(t)
	holdLease(t, a, labOne, claimOther)
	if out, err := runLease(a, "claim", labOne, "-p", "my proof", "--park"); Code(err) != ExitRefused || !strings.Contains(out, "parked on lease agentlab-1") {
		t.Fatalf("a parked claim: %q, exit %d", out, Code(err))
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s-sup")
	t.Setenv("CLAUDE_CODE_SESSION_NAME", claimSupervisor)
	if out, err := runLease(a, "revoke", labOne, "me"); err != nil || out != "revoked 1 grant(s) of agentlab-1\n" {
		t.Fatalf("the supervisor's revoke: %q, %v", out, err)
	}
	settleParks(t, a)
	settleParks(t, a)
	want := watchParty.Name + " → " + claimMe.Name + ": Your park on lease agentlab-1 (the lease agentlab-1: my proof) is over: agentlab-1 is held by \"Agent other\" " +
		"and no grant of it to you is left: ask the supervisor `need agentlab-1` again. Re-query the live state of your task and continue it."
	if !slices.Equal(*woke, []string{want}) {
		t.Errorf("woke %q\nwant %q", *woke, want)
	}
}
