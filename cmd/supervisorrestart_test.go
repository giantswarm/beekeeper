package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// TestLoopGivesTheMergeGoWhileItsRestartWaits checks that a restart into a
// replaced binary that waits for a claim in flight still runs the merge
// decision of an approved head, through a gate that ends at the merge, and
// nothing else of the pass (https://github.com/giantswarm/beekeeper/issues/3907).
func TestLoopGivesTheMergeGoWhileItsRestartWaits(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t) // the reviewer starts
	r.verdict("R1", loopHead, superloop.Approved, "")
	// beekeeper is installed anew while one of the loop's streams runs.
	r.replaced = true
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	if !l.BeginInFlight("stream:workers") {
		t.Fatal("failed to mark worker stream in flight")
	}
	alertReads := 0
	oldAlerts := readLoopAlerts
	t.Cleanup(func() { readLoopAlerts = oldAlerts })
	readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) {
		alertReads++
		return nil, nil
	}
	r.mu.Lock()
	r.calls = nil
	r.mu.Unlock()
	if err := l.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.MergeActions.Wait()
	if r.execs != 0 {
		t.Fatalf("re-executed while a claim was in flight: %d", r.execs)
	}
	gate := r.called(gateCmdName)
	if gate == nil || !slices.Contains(gate, loopHead) {
		t.Fatalf("no merge go while the restart waits: %v\n%s", r.calls, r.out)
	}
	if !slices.Contains(gate, "--until-merged") {
		t.Errorf("the merge's gate call waits for the release: %v", gate)
	}
	if alertReads != 0 {
		t.Errorf("the pass read alerts while its restart waits: %d", alertReads)
	}
	if c := r.called(agentsName, agentStartName); c != nil {
		t.Errorf("started %v while the restart waits", c)
	}
	if !strings.Contains(r.out.String(), "merge decisions go on meanwhile") {
		t.Errorf("the restart's line does not say the merges go on: %s", r.out)
	}
}

// TestLoopRestartWaitsAtMostSupervisorRestartWait checks that a claim in
// flight holds the restart into a replaced binary for supervisor.restartWait
// at most: past it the loop cancels the claim's action and re-executes
// (https://github.com/giantswarm/beekeeper/issues/3907).
func TestLoopRestartWaitsAtMostSupervisorRestartWait(t *testing.T) {
	r := newLoopRig(t)
	if err := yaml.Unmarshal([]byte("restartWait: 50ms"), &r.a.cfg.Supervisor); err != nil {
		t.Fatal(err)
	}
	r.replaced = true
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	// A merge whose gate waits for a release that does not come.
	key := "go " + loopPR + "@" + loopHead
	bgCtx := l.BackgroundContext(context.Background())
	if !l.BeginInFlight(key) {
		t.Fatal("failed to mark the merge in flight")
	}
	l.MergeActions.Go(func() {
		defer l.EndInFlight(key)
		<-bgCtx.Done()
	})
	if reexec, err := l.Reexec(context.Background()); err != nil || reexec || r.execs != 0 {
		t.Fatalf("restart with a claim in flight = %v, %v (execs=%d); want it held", reexec, err, r.execs)
	}
	if !strings.Contains(r.out.String(), "SUPERVISOR LOOP restart held: claims are in flight ("+key+")") {
		t.Fatalf("missing restart-held line naming the claim: %s", r.out)
	}
	time.Sleep(200 * time.Millisecond)
	if reexec, err := l.Reexec(context.Background()); err != nil || !reexec || r.execs != 1 {
		t.Fatalf("restart past supervisor.restartWait = %v, %v (execs=%d); want it re-executed", reexec, err, r.execs)
	}
	if bgCtx.Err() == nil {
		t.Error("the claim's action still runs after the re-execution")
	}
	if !strings.Contains(r.out.String(), "SUPERVISOR LOOP restart waited") {
		t.Errorf("missing the bounded restart's line: %s", r.out)
	}
}
