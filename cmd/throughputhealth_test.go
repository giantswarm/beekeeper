package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/giantswarm/beekeeper/internal/config"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func TestThroughputStreaksAndSuccesses(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	obs := throughputObservation{At: now}
	failure := throughputOutcome{At: now, Operation: "merge", Reason: "merge-command-exit-1", Candidate: "v1.2.3", URL: loopURL}
	outcomes := []throughputOutcome{failure, failure}
	check := func(want string) {
		t.Helper()
		got, _ := evaluateThroughput(now, 2*time.Hour, obs, outcomes, false)
		if got != want {
			t.Fatalf("got %s want %s", got, want)
		}
	}
	check("healthy")
	outcomes = append(outcomes, throughputOutcome{Operation: "start", Success: true}, failure)
	check("failed-merge-merge-command-exit-1")
	outcomes = append(outcomes, throughputOutcome{Operation: "merge", Success: true}, failure)
	check("healthy")
	outcomes = append(outcomes, failure, throughputOutcome{Operation: "merge", Reason: "different"})
	check("healthy")
	obs.Unknown = true
	check("unknown")
}

func TestThroughputEligibleWaitAndStale(t *testing.T) {
	now := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	obs := throughputObservation{At: now, Eligible: map[string]eligibleWait{loopPR: {Head: loopHead, URL: loopURL, Since: now.Add(-2 * time.Hour)}}}
	status, detail := evaluateThroughput(now, 2*time.Hour, obs, nil, false)
	if status != "stalled-merge" || !strings.Contains(detail, loopURL) || !strings.Contains(detail, "2h0m0s") {
		t.Fatal(status, detail)
	}
	status, _ = evaluateThroughput(now, 3*time.Hour, obs, nil, false)
	if status != "healthy" {
		t.Fatal(status)
	}
	status, _ = evaluateThroughput(now, 2*time.Hour, obs, nil, true)
	if status != "healthy" {
		t.Fatal(status)
	}
	obs.At = now.Add(-4 * time.Minute)
	status, _ = evaluateThroughput(now, 2*time.Hour, obs, nil, false)
	if status != "unknown" {
		t.Fatal(status)
	}
}

func TestThroughputRestartDedupRecoveryAndEscalation(t *testing.T) {
	r := newLoopRig(t)
	now := r.a.timeNow()
	obs := throughputObservation{At: now}
	if err := r.a.store.WriteFile(throughputObservationFile, obs); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := r.a.operationOutcome("merge", "merge-command-exit-1", loopURL, false); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want bool) {
		t.Helper()
		line, err := r.a.checkThroughput(now, false)
		if err != nil {
			t.Fatal(err)
		}
		if (line != "") != want {
			t.Fatalf("line %q want transition %v", line, want)
		}
	}
	check(true)
	// A new app, just as after restart, retains deduplication without memory.
	copyApp := *r.a
	r.a = &copyApp
	now = now.Add(time.Minute)
	obs.At = now
	_ = r.a.store.WriteFile(throughputObservationFile, obs)
	check(false)
	if err := r.a.operationOutcome("merge", "", loopURL, true); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	obs.At = now
	_ = r.a.store.WriteFile(throughputObservationFile, obs)
	check(true)
	now = now.Add(4 * time.Minute)
	check(true) // stale observation degrades rather than reporting recovery
	evs := r.logged(t, loopFileVerb)
	if len(evs) != 2 {
		t.Fatalf("guide reports: %d", len(evs))
	}
	for _, e := range evs {
		if !strings.Contains(e.Detail, "release throughput") {
			t.Fatal(e.Detail)
		}
	}
}

func TestThroughputObservationsTrackExactHeadAndRestart(t *testing.T) {
	r := newLoopRig(t)
	in := superloop.Input{Pulls: map[string]superloop.Pull{loopPR: {Head: loopHead, Green: true, URL: loopURL, Reviews: []superloop.Review{{Head: loopHead, Verdict: superloop.Approved}}}}}
	actions := []superloop.Action{{Kind: superloop.KindGo, Ref: loopPR, Head: loopHead}}
	if err := (&supervisorLoop{a: r.a}).observeThroughput(in, actions, false); err != nil {
		t.Fatal(err)
	}
	var old throughputObservation
	_, _ = r.a.store.ReadFile(throughputObservationFile, &old)
	r.a.now = r.a.timeNow().Add(time.Minute)
	if err := (&supervisorLoop{a: r.a}).observeThroughput(in, actions, false); err != nil {
		t.Fatal(err)
	}
	var next throughputObservation
	_, _ = r.a.store.ReadFile(throughputObservationFile, &next)
	if !next.Eligible[loopPR].Since.Equal(old.Eligible[loopPR].Since) {
		t.Fatal("lost wait")
	}
	p := in.Pulls[loopPR]
	p.Green = false
	in.Pulls[loopPR] = p
	_ = (&supervisorLoop{a: r.a}).observeThroughput(in, actions, false)
	next = throughputObservation{}
	_, _ = r.a.store.ReadFile(throughputObservationFile, &next)
	if len(next.Eligible) != 0 {
		t.Fatal("red CI eligible")
	}
}

func TestThroughputLoopAliveStillReports(t *testing.T) {
	r := newLoopRig(t)
	w := r.a.newWatcher(false, true)
	w.loopOwned = true
	w.now = r.a.timeNow()
	_ = r.a.store.WriteFile(throughputObservationFile, throughputObservation{At: w.now})
	for i := 0; i < 3; i++ {
		_ = r.a.operationOutcome("merge", "tooling", loopURL, false)
	}
	if !w.checkLoop(context.Background(), &state.State{}) {
		t.Fatal("loop should be alive")
	}
	if len(r.logged(t, loopFileVerb)) != 1 {
		t.Fatal("alive loop concealed failure")
	}
}

func TestThroughputPublicDiagnostics(t *testing.T) {
	for _, raw := range []string{"https://internal.invalid/o/r/pull/1", "https://user:secret@github.com/o/r/pull/1", "https://github.com/o/r/issues/1"} {
		if publicPullURL(raw) != "" {
			t.Fatal(raw)
		}
	}
	r := newLoopRig(t)
	if err := r.a.operationOutcome("start", "start-command-failed", loopURL, false); err != nil {
		t.Fatal(err)
	}
	var o throughputOutcome
	if err := json.Unmarshal([]byte(r.logged(t, throughputOutcomeVerb)[0].Detail), &o); err != nil {
		t.Fatal(err)
	}
	if o.At.IsZero() || o.Operation != "start" || o.Reason != "start-command-failed" {
		t.Fatal(o)
	}
}

func TestThroughputDetachedMergeDurableReasons(t *testing.T) {
	r := newLoopRig(t)
	now := r.a.timeNow()
	for i := 0; i < 3; i++ {
		st := &state.State{Merges: []state.Merge{{Repo: "o/r", PR: i + 1, By: loopParty, Phase: state.Running}}}
		events := recordRun(st, 0, config.Lane{Name: "o/r"}, loopParty, runOutcome{rc: 7, reason: fmt.Sprintf("tool failed with incidental text %d", i)}, now, "")
		if err := r.a.store.Record(events...); err != nil {
			t.Fatal(err)
		}
	}
	_ = r.a.store.WriteFile(throughputObservationFile, throughputObservation{At: now})
	line, err := r.a.checkThroughput(now, false)
	if err != nil || !strings.Contains(line, "failed-merge-merge-command-exit-7") {
		t.Fatal(line, err)
	}
	if strings.Contains(line, "incidental") {
		t.Fatal(line)
	}
	if len(r.logged(t, throughputOutcomeVerb)) != 3 {
		t.Fatal("missing detached outcomes")
	}
}

func TestThroughputStartChangingTextAndIntentionalRefusals(t *testing.T) {
	r := newLoopRig(t)
	l := &supervisorLoop{a: r.a}
	for i := 0; i < 3; i++ {
		r.answer = func([]string) (string, int) { return fmt.Sprintf("start failed, incidental %d", i), 1 }
		if _, err := l.runStart(context.Background(), superloop.Action{URL: loopURL}, agentsName, agentStartName, "worker", "brief"); err == nil {
			t.Fatal("expected failure")
		}
	}
	r.answer = func([]string) (string, int) { return "capacity held", ExitRefused }
	_, _ = l.runStart(context.Background(), superloop.Action{}, agentsName, agentStartName, "worker", "brief")
	if len(r.logged(t, throughputOutcomeVerb)) != 3 {
		t.Fatal("intentional refusal counted")
	}
	_ = r.a.store.WriteFile(throughputObservationFile, throughputObservation{At: r.a.timeNow()})
	line, err := r.a.checkThroughput(r.a.timeNow(), false)
	if err != nil || !strings.Contains(line, "failed-start-start-command-failed") {
		t.Fatal(line, err)
	}
}

func TestThroughputStallEscalatesAndRecoversAfterUnknown(t *testing.T) {
	r := newLoopRig(t)
	now := r.a.timeNow()
	obs := throughputObservation{At: now, Eligible: map[string]eligibleWait{loopPR: {Head: loopHead, URL: loopURL, Since: now}}}
	check := func() string {
		t.Helper()
		if err := r.a.store.WriteFile(throughputObservationFile, obs); err != nil {
			t.Fatal(err)
		}
		line, err := r.a.checkThroughput(now, false)
		if err != nil {
			t.Fatal(err)
		}
		return line
	}
	if line := check(); line != "" {
		t.Fatal(line)
	}
	now = now.Add(2 * time.Hour)
	obs.At = now
	if line := check(); !strings.Contains(line, "stalled-merge") {
		t.Fatal(line)
	}
	now = now.Add(time.Minute)
	obs.At = now
	if line := check(); line != "" {
		t.Fatal("duplicate", line)
	}
	now = now.Add(4 * time.Minute)
	if line := check(); !strings.Contains(line, "unknown") {
		t.Fatal(line)
	}
	now = now.Add(time.Minute)
	obs.At = now
	obs.Eligible = nil
	if line := check(); !strings.Contains(line, "RECOVERED") {
		t.Fatal(line)
	}
	if len(r.logged(t, loopFileVerb)) != 2 {
		t.Fatal("incident and recovery must each be filed once")
	}
}
