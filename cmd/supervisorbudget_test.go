package cmd

import (
	"strings"
	"testing"
	"time"
)

// This regression only uses the pre-existing loop rig, so it can also run
// against the unchanged base implementation.
func TestLoopBudgetHoldsNewStartsUntilRecovery(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	if err := r.a.store.Record(event(watchParty, "watch.budget-state", "true")); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	if r.called(agentsName, agentStartName) != nil {
		t.Fatal("started reviewer under budget floor")
	}
	if !strings.Contains(r.out.String(), "SUPERVISOR STARTS HELD") {
		t.Fatal("budget hold absent from loop log")
	}
	if err := r.a.store.Record(event(watchParty, "watch.budget-state", "false")); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	if r.called(agentsName, agentStartName) == nil {
		t.Fatal("budget recovery did not release reviewer start")
	}
}
