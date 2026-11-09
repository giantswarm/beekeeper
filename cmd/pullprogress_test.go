package cmd

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
	"github.com/giantswarm/beekeeper/internal/update"
)

// The history reader has finished, but a cold batch of individually healthy
// PR reads can still outlast probation's heartbeat window. Exercise the real
// pass and recovery signals, without wall-clock sleeps or a background heartbeat.
func TestRecoveryAcceptsProgressingPullBatchAndRejectsBlockedRead(t *testing.T) {
	r := newLoopRig(t)
	c, now := recoveryRig(t)
	store, err := state.Open(r.a.store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	c.store, c.a.store = store, store
	j := journal(t, c)
	oldClock, oldVersion, oldList := loopLifecycleNow, loopRunningVersion, readOpenPulls
	t.Cleanup(func() {
		loopLifecycleNow, loopRunningVersion, readOpenPulls = oldClock, oldVersion, oldList
	})
	loopLifecycleNow = func() time.Time { return *now }
	loopRunningVersion = func() string { return recoveryBadVersion }
	refs := make([]string, 12)
	for i := range refs {
		refs[i] = fmt.Sprintf("o/r#%d", i+100)
	}
	readOpenPulls = func(context.Context, func() ([]string, error), []string) ([]string, error) {
		return refs, nil
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.once = true
	l.lifecycle = &loopLifecycle{Version: recoveryBadVersion, Started: *now}
	reads := 0
	readLoopPull = func(ctx context.Context, ref string) (superloop.Pull, error) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > time.Minute {
			t.Fatal("PR observation has no bounded deadline")
		}
		reads++
		*now = now.Add(time.Minute)
		s := c.signals(ctx, j)
		if !s.Heartbeat {
			t.Fatalf("healthy read %d lost heartbeat after history completed: %+v", reads, s)
		}
		if reads == len(refs) {
			// An individual read that stops advancing must still roll back;
			// merely entering this phase cannot keep it healthy indefinitely.
			*now = now.Add(4 * time.Minute)
			s = c.signals(ctx, j)
			if verdict, reason := update.Decide(update.Observed{Elapsed: update.HeartbeatGrace, ProbePassed: true}, s); verdict != update.RollBack {
				t.Fatalf("blocked PR read: %s: %s", verdict, reason)
			}
			if reason := c.loopSilence(j.Candidate.Version, j.InstalledAt); reason != "candidate loop last reached observe pull requests; no health checkpoint for 5m0s" {
				t.Fatalf("misleading blocked-read diagnostic: %s", reason)
			}
		}
		return superloop.Pull{Ref: ref, State: superloop.Open, Draft: true, Author: loopDesk}, nil
	}
	t.Cleanup(func() {
		l.passActions.Wait()
		l.mergeActions.Wait()
		l.timerActs.Wait()
	})
	if err := l.pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if reads != len(refs) {
		t.Fatalf("read %d of %d PRs", reads, len(refs))
	}
}

// Admission can block before readLoopPull's own timeout starts. A caller's
// cancellation must release that wait without issuing a GitHub observation.
func TestPullObservationCancellationBeforeRead(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.GitHub.Floor = 2500
	healthyLoopBudget(t, r)
	l := &supervisorLoop{a: r.a}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	readLoopPull = func(context.Context, string) (superloop.Pull, error) {
		t.Fatal("canceled observation reached GitHub")
		return superloop.Pull{}, nil
	}
	_, err := l.observePull(ctx, loopPR, time.Time{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled observation: %v", err)
	}
}
