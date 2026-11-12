package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
)

const lifecycleFirstPass = "first pass"
const lifecycleWatchAlerts = "watch alerts"

func TestRecoveryReportsCandidateExitInsteadOfBareSilence(t *testing.T) {
	c, now := recoveryRig(t)
	if err := c.store.WriteFile(throughputObservationFile, throughputObservation{At: now.Add(-time.Minute), Candidate: recoveryGoodVersion}); err != nil {
		t.Fatal(err)
	}
	// This is an exit from the actual candidate, not the isolated probe or the old loop.
	if err := c.store.WriteFile(loop.LoopLifecycleFile, loop.LoopLifecycle{Version: recoveryBadVersion, Started: *now, At: *now, Phase: lifecycleFirstPass, Exited: true, Code: 1, Failure: "state lock busy"}); err != nil {
		t.Fatal(err)
	}
	// The candidate loop started at the first tick: its grace ends one tick later.
	for range int(update.HeartbeatGrace/time.Minute) + 1 {
		c.mustTick(t)
	}
	reason := journal(t, c).Observed.Reason
	if !strings.Contains(reason, "state lock busy") || !strings.Contains(reason, lifecycleFirstPass) || !strings.Contains(reason, "exit 1") {
		t.Fatalf("rollback hid candidate failure: %s", reason)
	}
}

func TestRecoveryRejectsStaleOrMismatchedPhaseProgress(t *testing.T) {
	for _, tc := range []struct {
		name, version, phase string
		age                  time.Duration
		exited               bool
		want                 string
	}{
		{"stalled first pass", recoveryBadVersion, lifecycleWatchAlerts, 0, false, "last reached watch alerts"},
		{"old binary", recoveryGoodVersion, lifecycleFirstPass, 0, true, "no matching candidate"},
		{"before install", recoveryBadVersion, lifecycleFirstPass, -time.Minute, true, "no matching candidate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, now := recoveryRig(t)
			if err := c.store.WriteFile(throughputObservationFile, throughputObservation{At: now.Add(-time.Minute), Candidate: recoveryGoodVersion}); err != nil {
				t.Fatal(err)
			}
			life := loop.LoopLifecycle{Version: tc.version, Started: now.Add(tc.age), At: *now, Phase: tc.phase, Exited: tc.exited, Code: 1}
			if err := c.store.WriteFile(loop.LoopLifecycleFile, life); err != nil {
				t.Fatal(err)
			}
			for range int(update.HeartbeatGrace/time.Minute) + 1 {
				c.mustTick(t)
				*now = now.Add(time.Minute)
			}
			j := journal(t, c)
			if !j.RolledBack() || !strings.Contains(j.Observed.Reason, tc.want) {
				t.Fatalf("lifecycle changed verdict or hid phase: %+v", j)
			}
		})
	}
}

func TestRecoveryProgressingFirstPassPassesProbation(t *testing.T) {
	c, now := recoveryRig(t)
	started := *now
	// No completed-pass observation exists. Progress through the first pass
	// continues beyond both the heartbeat grace and the probation window.
	for i := range int(update.ProbationWindow / time.Minute) {
		*now = now.Add(time.Minute)
		phase := loop.LoopPhaseObserve
		if i%2 == 0 {
			phase = "read event history"
		}
		if err := c.store.WriteFile(loop.LoopLifecycleFile, loop.LoopLifecycle{Version: recoveryBadVersion, Started: started, At: now.Add(-4 * time.Second), Phase: phase}); err != nil {
			t.Fatal(err)
		}
		settled := c.mustTick(t)
		if settled != (i == int(update.ProbationWindow/time.Minute)-1) {
			t.Fatalf("unexpected verdict at minute %d: %+v", i+1, journal(t, c))
		}
	}
	if installed(t, c) != recoveryBadBytes || journal(t, c).Pending() {
		t.Fatal("a progressing first pass was rolled back or left unproven")
	}
}

func TestLoopPhasePublishesHeartbeatForRetainedController(t *testing.T) {
	c, _ := recoveryRig(t)
	previous := loop.LoopRunningVersion
	t.Cleanup(func() { loop.LoopRunningVersion = previous })
	loop.LoopRunningVersion = func() string { return recoveryBadVersion }
	l := &loop.Loop{Host: c.a, Lifecycle: &loop.LoopLifecycle{Version: recoveryBadVersion, Started: time.Now().UTC()}}
	l.LifecyclePhase(loop.LoopPhaseObserve)
	var obs throughputObservation
	if found, err := c.store.ReadFile(throughputObservationFile, &obs); err != nil || !found {
		t.Fatalf("phase heartbeat missing: %v", err)
	}
	if obs.Candidate != recoveryBadVersion || !obs.At.Equal(l.Lifecycle.At) || !obs.Unknown {
		t.Fatalf("phase progress incompatible with retained controller: %+v", obs)
	}
	// Phase heartbeats must retain a completed planner observation.
	obs.Unknown = false
	if err := c.store.WriteFile(throughputObservationFile, obs); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"reload configuration", "watch conditions", lifecycleWatchAlerts, loop.LoopPhaseBetween} {
		l.LifecyclePhase(phase)
		if _, err := c.store.ReadFile(throughputObservationFile, &obs); err != nil || obs.Unknown {
			t.Fatalf("phase %s erased completed planning: %+v %v", phase, obs, err)
		}
	}
}

func TestRecoveryFailedExitOverridesFreshPhaseHeartbeat(t *testing.T) {
	c, now := recoveryRig(t)
	if err := c.store.WriteFile(throughputObservationFile, throughputObservation{At: *now, Candidate: recoveryBadVersion}); err != nil {
		t.Fatal(err)
	}
	if err := c.store.WriteFile(loop.LoopLifecycleFile, loop.LoopLifecycle{Version: recoveryBadVersion, Started: *now, At: *now, Phase: lifecycleFirstPass, Exited: true, Code: 1}); err != nil {
		t.Fatal(err)
	}
	if s := c.signals(t.Context(), journal(t, c)); s.Heartbeat || !s.HeartbeatRequired {
		t.Fatalf("a failed exit was counted as progress: %+v", s)
	}
}

func TestRecoveryRollbackAutomaticallyReleasesBroadFence(t *testing.T) {
	c, now := recoveryRig(t)
	c.failMerges(t, 3, *now)
	if !c.mustTick(t) || !journal(t, c).RolledBack() {
		t.Fatal("rollback did not finish")
	}
	var fence struct {
		Recovered bool `json:"recovered"`
	}
	if found, err := c.store.ReadFile("recovery-writer.json", &fence); err != nil || !found || !fence.Recovered {
		t.Fatalf("rollback needs manual recovery: %+v %v", fence, err)
	}
	// A new controller must also reconcile an already-settled rollback.
	if !c.mustTick(t) {
		t.Fatal("settled rollback was not idempotent")
	}
}

// The old release's loop re-executes into the candidate only at its next quiet
// pass: the candidate's grace starts there, not at the install (issue 3345).
func TestRecoveryGraceStartsWithALateCandidateLoop(t *testing.T) {
	c, now := recoveryRig(t)
	if err := c.store.WriteFile(throughputObservationFile, throughputObservation{At: now.Add(-time.Minute), Candidate: recoveryGoodVersion}); err != nil {
		t.Fatal(err)
	}
	// The old loop still runs: this process stands in for it.
	if err := c.store.WriteFile(loop.LoopVersionFile, loop.LoopVersionRecord{Version: recoveryGoodVersion, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	handover := int(update.HeartbeatGrace/time.Minute) + 5
	for range handover {
		c.mustTick(t)
	}
	if installed(t, c) != recoveryBadBytes {
		t.Fatalf("rolled back before the candidate's loop ran: %s", journal(t, c).Observed.Reason)
	}
	// The candidate's loop starts now and stalls in its first pass.
	if err := c.store.WriteFile(loop.LoopLifecycleFile, loop.LoopLifecycle{Version: recoveryBadVersion, Started: *now, At: *now, Phase: lifecycleFirstPass}); err != nil {
		t.Fatal(err)
	}
	if err := c.store.WriteFile(loop.LoopVersionFile, loop.LoopVersionRecord{Version: recoveryBadVersion, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	for range int(update.HeartbeatGrace / time.Minute) {
		c.mustTick(t)
		if installed(t, c) != recoveryBadBytes {
			t.Fatalf("rolled back within the candidate loop's grace: %s", journal(t, c).Observed.Reason)
		}
		*now = now.Add(time.Minute)
	}
	c.mustTick(t)
	j := journal(t, c)
	if !j.RolledBack() || !strings.Contains(j.Observed.Reason, "last reached "+lifecycleFirstPass) {
		t.Fatalf("a stalled candidate loop outlived its grace: %+v", j.Observed)
	}
}

// An old loop that never hands over still ends the probation: the candidate
// is rolled back at the window, naming the loop that held on.
func TestRecoveryRollsBackWhenTheOldLoopNeverHandsOver(t *testing.T) {
	c, now := recoveryRig(t)
	if err := c.store.WriteFile(throughputObservationFile, throughputObservation{At: now.Add(-time.Minute), Candidate: recoveryGoodVersion}); err != nil {
		t.Fatal(err)
	}
	if err := c.store.WriteFile(loop.LoopVersionFile, loop.LoopVersionRecord{Version: recoveryGoodVersion, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	ticks := 0
	for installed(t, c) == recoveryBadBytes && ticks < 2*int(update.ProbationWindow/time.Minute) {
		c.mustTick(t)
		ticks++
	}
	if ticks != int(update.ProbationWindow/time.Minute) {
		t.Fatalf("rolled back after %d ticks", ticks)
	}
	if reason := journal(t, c).Observed.Reason; !strings.Contains(reason, "another-version loop is still alive") {
		t.Fatalf("reason hid the old loop: %s", reason)
	}
}

func TestRecoveryLateLoopGraceExcludesUnobservedSleep(t *testing.T) {
	c, now := recoveryRig(t)
	if err := c.store.WriteFile(throughputObservationFile, throughputObservation{At: now.Add(-time.Minute), Candidate: recoveryGoodVersion}); err != nil {
		t.Fatal(err)
	}
	if err := c.store.WriteFile(loop.LoopVersionFile, loop.LoopVersionRecord{Version: recoveryGoodVersion, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	for range 15 {
		c.mustTick(t)
		*now = now.Add(time.Minute)
	}
	if err := c.store.WriteFile(loop.LoopLifecycleFile, loop.LoopLifecycle{Version: recoveryBadVersion, Started: *now, At: *now, Phase: lifecycleFirstPass}); err != nil {
		t.Fatal(err)
	}
	// The loop starts just before sleep, without an intervening controller tick.
	*now = now.Add(time.Hour)
	c.mustTick(t)
	o := journal(t, c).Observed
	if installed(t, c) != recoveryBadBytes || !o.LoopStarted || o.LoopStartedAt != 15*time.Minute {
		t.Fatalf("unobserved sleep consumed the late loop's grace: %+v", o)
	}
	for range int(update.HeartbeatGrace/time.Minute) - 2 {
		c.mustTick(t)
		if installed(t, c) != recoveryBadBytes {
			t.Fatalf("rolled back within observed grace: %+v", journal(t, c).Observed)
		}
	}
	c.mustTick(t)
	if !journal(t, c).RolledBack() {
		t.Fatal("silent loop outlived its observed grace")
	}
}

func TestLoopExitPersistsDiagnosticWithoutErrorContents(t *testing.T) {
	r := newLoopRig(t)
	r.failRecord = func(e state.Event) bool { return e.Verb == loop.LoopStartVerb }
	if err := r.a.runSupervisorLoop(t.Context(), true); err == nil {
		t.Fatal("expected injected event write failure")
	}
	var life loop.LoopLifecycle
	found, err := r.a.store.ReadFile(loop.LoopLifecycleFile, &life)
	if err != nil || !found || !life.Exited || life.Code != 1 || life.Phase != loop.LoopPhaseStartupHistory {
		t.Fatalf("lost startup exit: %+v, %v", life, err)
	}
	if strings.Contains(life.Failure, "disk full") {
		t.Fatal("recorded arbitrary error text")
	}
}

func TestRecoveryPromotionClearsCompatibleFence(t *testing.T) {
	c, _ := recoveryRig(t)
	// A failed earlier prerelease left a fence; the next compatible signed
	// candidate must finish real probation before that fence disappears.
	if err := c.store.RecoverVersion(recoveryGoodVersion, recoveryGoodVersion, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	for range int(update.ProbationWindow/time.Minute) - 1 {
		if c.mustTick(t) {
			t.Fatal("promoted early")
		}
	}
	var fence map[string]any
	if found, err := c.store.ReadFile("recovery-writer.json", &fence); err != nil || !found {
		t.Fatalf("fence disappeared before probation: %v", err)
	}
	if !c.mustTick(t) {
		t.Fatal("healthy candidate did not settle")
	}
	if found, err := c.store.ReadFile("recovery-writer.json", &fence); err != nil || found {
		t.Fatalf("passed candidate retained fence: %v", err)
	}
}

func TestLoopFailureClassificationDoesNotExposePrivateErrorText(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{state.ErrRecoveryWriterFenced, "state writes fenced after binary recovery"},
		{&state.LockBusyError{Command: "private command"}, "state lock busy"},
		{&os.PathError{Op: "open", Path: "private path", Err: os.ErrPermission}, "permission denied"},
		{errors.New("private error text"), "loop error (see candidate service journal)"},
	} {
		if got := loop.LoopFailureKind(tc.err); got != tc.want {
			t.Fatalf("got %q want %q", got, tc.want)
		}
	}
}

func TestSecondLoopCannotReplaceOwnerLifecycle(t *testing.T) {
	r := newLoopRig(t)
	owner := loop.LoopLifecycle{Version: recoveryBadVersion, Started: time.Now(), Phase: lifecycleWatchAlerts}
	if err := r.a.store.WriteFile(loop.LoopLifecycleFile, owner); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(filepath.Join(r.a.store.Dir(), loop.LoopLockFile))
	if held, err := lock.TryLock(); !held || err != nil {
		t.Fatalf("lock: %v %v", held, err)
	}
	defer func() { _ = lock.Unlock() }()
	if err := r.a.runSupervisorLoop(t.Context(), true); Code(err) != ExitRefused {
		t.Fatalf("second loop: %v", err)
	}
	var got loop.LoopLifecycle
	if _, err := r.a.store.ReadFile(loop.LoopLifecycleFile, &got); err != nil || got.Exited || got.Phase != owner.Phase || !got.Started.Equal(owner.Started) {
		t.Fatalf("contender overwrote owner: %+v %v", got, err)
	}
}

func TestRecoveryIdentifiesIncumbentThatNeverHandedOff(t *testing.T) {
	c, now := recoveryRig(t)
	if err := c.store.WriteFile(loop.LoopVersionFile, loop.LoopVersionRecord{PID: os.Getpid(), Version: recoveryGoodVersion}); err != nil {
		t.Fatal(err)
	}
	if got := c.loopSilence(recoveryBadVersion, *now); !strings.Contains(got, "another-version loop is still alive") {
		t.Fatalf("lost incumbent evidence: %s", got)
	}
}

func TestLoopSuccessfulExitHasZeroDiagnosticCode(t *testing.T) {
	r := newLoopRig(t)
	r.pass(t)
	var life loop.LoopLifecycle
	if found, err := r.a.store.ReadFile(loop.LoopLifecycleFile, &life); err != nil || !found || !life.Exited || life.Code != 0 || life.Failure != "" {
		t.Fatalf("successful loop recorded failure: %+v %v", life, err)
	}
}

func TestLoopPanicIsRecordedAndPropagated(t *testing.T) {
	r := newLoopRig(t)
	r.failRecord = func(e state.Event) bool {
		if e.Verb == loop.LoopStartVerb {
			panic("private panic detail")
		}
		return false
	}
	func() {
		defer func() {
			if recovered := recover(); recovered != "private panic detail" {
				t.Fatalf("panic changed: %v", recovered)
			}
		}()
		_ = r.a.runSupervisorLoop(t.Context(), true)
		t.Fatal("panic swallowed")
	}()
	var life loop.LoopLifecycle
	if found, err := r.a.store.ReadFile(loop.LoopLifecycleFile, &life); err != nil || !found || !life.Exited || life.Code != 1 || life.Failure != "panic (see candidate service journal)" {
		t.Fatalf("panic recorded incorrectly: %+v %v", life, err)
	}
}

// Model the retained controller's install-based grace: a late first pass must
// report progress without waiting for event history or a completed plan.
func TestRecoveryAcceptsHistoryCheckpointAndRejectsStalledReader(t *testing.T) {
	c, now := recoveryRig(t)
	oldVersion := loop.LoopRunningVersion
	t.Cleanup(func() { loop.LoopRunningVersion = oldVersion })
	loop.LoopRunningVersion = func() string { return recoveryBadVersion }
	j := journal(t, c)
	*now = now.Add(update.HeartbeatGrace)
	l := &loop.Loop{Host: c.a}
	if err := l.CheckpointThroughput(*now); err != nil {
		t.Fatal(err)
	}
	o := update.Observed{Elapsed: update.HeartbeatGrace, ProbePassed: true}
	s := c.signals(t.Context(), j)
	if !s.Heartbeat || !s.HeartbeatRequired || s.Unproven != "" {
		t.Fatalf("checkpoint not recognized: %+v", s)
	}
	if verdict, reason := update.Decide(o, s); verdict != update.Wait {
		t.Fatalf("late history load: %s: %s", verdict, reason)
	}
	*now = now.Add(4 * time.Minute)
	s = c.signals(t.Context(), j)
	if verdict, reason := update.Decide(o, s); verdict != update.RollBack {
		t.Fatalf("stalled history reader: %s: %s", verdict, reason)
	}
}

// Even discarded events prove that the reader is moving. Exercise the real
// Follow callback path across more than a heartbeat grace, then stop reading.
func TestRecoveryAcceptsProgressingEventHistory(t *testing.T) {
	c, now := recoveryRig(t)
	oldVersion := loop.LoopRunningVersion
	t.Cleanup(func() { loop.LoopRunningVersion = oldVersion })
	loop.LoopRunningVersion = func() string { return recoveryBadVersion }
	j := journal(t, c)
	events := make([]state.Event, 20)
	for i := range events {
		events[i] = state.Event{At: *now, Verb: "history-fixture"}
	}
	if err := c.store.Update(func(*state.State) ([]state.Event, error) { return events, nil }); err != nil {
		t.Fatal(err)
	}
	l := &loop.Loop{Host: c.a}
	checkpoints := 0
	filter := loop.EventProgressFilter(func(state.Event) bool { return false }, func() {
		checkpoints++
		if err := l.CheckpointThroughput(*now); err != nil {
			t.Fatal(err)
		}
		s := c.signals(t.Context(), j)
		if verdict, reason := update.Decide(update.Observed{Elapsed: update.HeartbeatGrace, ProbePassed: true}, s); verdict != update.Wait {
			t.Fatalf("progressing reader: %s: %s", verdict, reason)
		}
	}, func() time.Time { return *now })
	kept, err := c.store.Follow("history-progress-test", func(e state.Event) bool {
		*now = now.Add(time.Minute)
		return filter(e)
	})
	if err != nil || len(kept) != 0 || checkpoints < len(events) {
		t.Fatalf("history: kept=%d checkpoints=%d err=%v", len(kept), checkpoints, err)
	}
	*now = now.Add(4 * time.Minute)
	if verdict, reason := update.Decide(update.Observed{Elapsed: update.HeartbeatGrace, ProbePassed: true}, c.signals(t.Context(), j)); verdict != update.RollBack {
		t.Fatalf("stalled reader: %s: %s", verdict, reason)
	}
}

func TestEventHistoryProgressIsRateLimited(t *testing.T) {
	now := time.Now()
	checkpoints := 0
	filter := loop.EventProgressFilter(func(e state.Event) bool { return e.Verb == answerRecorded }, func() { checkpoints++ }, func() time.Time { return now })
	for range 10 {
		if filter(state.Event{}) {
			t.Fatal("discarded event retained")
		}
	}
	if checkpoints != 0 {
		t.Fatal("checkpoint before cadence")
	}
	now = now.Add(throughputCadence)
	if !filter(state.Event{Verb: answerRecorded}) {
		t.Fatal("accepted event discarded")
	}
	for range 10 {
		filter(state.Event{})
	}
	if checkpoints != 1 {
		t.Fatalf("checkpoints=%d, want 1", checkpoints)
	}
}

// A phase can take longer than the freshness bound on a loaded machine. Its
// grace must follow the last checkpoint, even after the loop is ten minutes old.
func TestRecoveryGraceFollowsLastCheckpoint(t *testing.T) {
	for _, useLifecycle := range []bool{false, true} {
		name := "throughput"
		if useLifecycle {
			name = "lifecycle"
		}
		t.Run(name, func(t *testing.T) {
			c, now := recoveryRig(t)
			started := *now
			for minute := 1; minute <= 21; minute++ {
				*now = started.Add(time.Duration(minute) * time.Minute)
				if minute <= 11 {
					if useLifecycle {
						if err := c.store.WriteFile(loop.LoopLifecycleFile, loop.LoopLifecycle{Version: recoveryBadVersion, Started: started, At: *now, Phase: loop.LoopPhaseRecover}); err != nil {
							t.Fatal(err)
						}
					} else if err := c.store.WriteFile(throughputObservationFile, throughputObservation{Candidate: recoveryBadVersion, At: *now}); err != nil {
						t.Fatal(err)
					}
				}
				settled := c.mustTick(t)
				if settled != (minute == 21) {
					t.Fatalf("minute %d: settled=%v journal=%+v", minute, settled, journal(t, c))
				}
			}
			j := journal(t, c)
			if !j.RolledBack() || j.Observed.LastBeatAt != 11*time.Minute {
				t.Fatalf("did not roll back ten minutes after last checkpoint: %+v", j)
			}
		})
	}
}
