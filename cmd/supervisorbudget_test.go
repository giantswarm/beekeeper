package cmd

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// This regression only uses the pre-existing loop rig, so it can also run
// against the unchanged base implementation.
func TestLoopBudgetHoldsNewStartsUntilRecovery(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	held := true
	r.answer = func(args []string) (string, int) {
		if slices.Equal(args, []string{"budget", "--gate"}) && held {
			return "under floor", ExitRefused
		}
		return "", 0
	}
	r.pass(t)
	if r.called("budget", "--gate") == nil {
		t.Fatal("idle poll did not read GitHub budget")
	}
	if r.called(agentsName, agentStartName) != nil {
		t.Fatal("started reviewer under budget floor")
	}
	if !strings.Contains(r.out.String(), "SUPERVISOR STARTS HELD") {
		t.Fatal("budget hold absent from loop log")
	}
	held = false
	r.pass(t)
	if r.called("budget", "--gate") == nil {
		t.Fatal("next poll did not refresh GitHub budget")
	}
	if r.called(agentsName, agentStartName) == nil {
		t.Fatal("budget recovery did not release reviewer start")
	}
}
