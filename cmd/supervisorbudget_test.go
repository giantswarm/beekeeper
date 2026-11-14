package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/watch"
)

// This regression only uses the pre-existing loop rig, so it can also run
// against the unchanged base implementation.
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestLoopBudgetHoldsNewStartsUntilRecovery(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	if err := r.a.store.Record(event(watch.WatchParty, "watch.budget-state", "true")); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	if r.called(agentsName, agentStartName) != nil {
		t.Fatal("started reviewer under budget floor")
	}
	if !strings.Contains(r.out.String(), "SUPERVISOR STARTS HELD") {
		t.Fatal("budget hold absent from loop log")
	}
	if err := r.a.store.Record(event(watch.WatchParty, "watch.budget-state", "false")); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	if r.called(agentsName, agentStartName) == nil {
		t.Fatal("budget recovery did not release reviewer start")
	}
}
