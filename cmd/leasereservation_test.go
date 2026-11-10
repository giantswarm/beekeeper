package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestLeaseWakeReservesFullWindow(t *testing.T) {
	a := claimApp(t, claimGrant(labTwo, claimMe, 29*time.Minute))
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	answer, ready := a.leaseSettled(st, state.Agent{Party: claimMe}, labTwo)
	if !ready || !strings.Contains(answer, "free and yours") {
		t.Fatalf("wake: %q, %v", answer, ready)
	}
	st.Agents = []state.Agent{{Party: claimMe, Park: &state.Park{On: leaseParkOn(labTwo)}}}
	markResumable(st, []settledPark{{party: claimMe, on: leaseParkOn(labTwo), answer: answer}}, true, a.now)
	later := a.now.Add(29 * time.Minute)
	gate := lease.Gate{Resource: labTwo, Caller: claimOther, Now: later, TTL: 30 * time.Minute}
	if _, err := lease.Check(st, gate); err == nil {
		t.Fatal("outsider bypassed reservation")
	}
	gate.Caller = claimMe
	if idx, err := lease.Check(st, gate); err != nil || idx < 0 {
		t.Fatalf("head lost reservation: %d, %v", idx, err)
	}
	gate.Caller, gate.Now = claimOther, a.now.Add(31*time.Minute)
	if _, err := lease.Check(st, gate); err != nil {
		t.Fatalf("reservation did not expire: %v", err)
	}
}

func TestExpiredGrantNeverPromisesLease(t *testing.T) {
	a := claimApp(t, claimGrant(labTwo, claimMe, time.Hour))
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	answer, ready := a.leaseSettled(st, state.Agent{Party: claimMe}, labTwo)
	if !ready || strings.Contains(answer, "free and yours") || !strings.Contains(answer, "queues you again") {
		t.Fatalf("expired wake: %q, %v", answer, ready)
	}
}

func TestLeaseWakeRechecksQueue(t *testing.T) {
	a := claimApp(t, claimGrant(labTwo, claimFirst, time.Minute))
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	parks := []settledPark{{party: claimMe, on: leaseParkOn(labTwo), answer: "previous queue observation"}}
	if got := a.recheckLeaseParks(st, parks); len(got) != 0 {
		t.Fatalf("stale queue wake: %+v", got)
	}
}

func TestLabPlatformPID(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{agentlabCmd, labPlatformCommand, labSelectorFlag, labOne}, 42},
		{[]string{"/bin/agentlab", "--lab=" + labOne, labPlatformCommand}, 42},
		{[]string{agentlabCmd, labPlatformCommand}, 42},
		{[]string{agentlabCmd, labPlatformCommand, labSelectorFlag, labTwo}, 0},
		{[]string{agentlabCmd, executionStatus, labSelectorFlag, labOne}, 0},
		{[]string{"sh", "-c", "agentlab platform"}, 0},
	} {
		table := proc.NewTable(&proc.Process{PID: 42, Args: tc.args})
		if got := labPlatformPID(table, labOne, labCluster); got != tc.want {
			t.Errorf("%v: got %d, want %d", tc.args, got, tc.want)
		}
	}
}

// A platform restore remains a release barrier even if the worker explicitly
// releases (or forces release of) the lab before its background command exits.
func TestLabReleaseWaitsForPlatformExit(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "holder release", true: "forced"}[force], func(t *testing.T) {
			a := claimApp(t, claimGrant(labOne, claimFirst, time.Minute))
			holdLease(t, a, labOne, claimMe)
			machine := restoreMachine{Machine: plat.Machine, table: proc.NewTable(&proc.Process{PID: 42, Args: []string{agentlabCmd, labPlatformCommand, labSelectorFlag, labOne}})}
			plat.Machine = machine
			args := []string{"release", labOne}
			if force {
				args = append(args, "--force")
			}
			if _, err := runLease(a, args...); Code(err) != ExitRefused || !strings.Contains(err.Error(), "still running") {
				t.Fatalf("release during restore: %v", err)
			}
			if h, err := lease.Dir(a.cfg.LeaseDir).Get(labOne); err != nil || h == nil {
				t.Fatalf("lease released during restore: %v, %v", h, err)
			}
			machine.table = proc.NewTable()
			plat.Machine = machine
			if out, err := runLease(a, args...); err != nil || !strings.HasPrefix(out, "released ") {
				t.Fatalf("release after exit: %q, %v", out, err)
			}
		})
	}
}

type restoreMachine struct {
	platform.Machine
	table *proc.Table
}

func (m restoreMachine) Processes() (*proc.Table, error) { return m.table, nil }
