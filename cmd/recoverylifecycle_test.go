package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
)

const lifecycleFirstPass = "first pass"

func TestRecoveryReportsCandidateExitInsteadOfBareSilence(t *testing.T) {
	c, now := recoveryRig(t)
	if err := c.store.WriteFile(throughputObservationFile, throughputObservation{At: now.Add(-time.Minute), Candidate: recoveryGoodVersion}); err != nil {
		t.Fatal(err)
	}
	// This is an exit from the actual candidate, not the isolated probe or the old loop.
	if err := c.store.WriteFile(loopLifecycleFile, loopLifecycle{Version: recoveryBadVersion, Started: *now, At: *now, Phase: lifecycleFirstPass, Exited: true, Code: 1, Failure: "state lock busy"}); err != nil {
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

func TestRecoveryLifecycleNeverSubstitutesForHeartbeat(t *testing.T) {
	for _, tc := range []struct {
		name, version, phase string
		age                  time.Duration
		exited               bool
		want                 string
	}{
		{"stalled first pass", recoveryBadVersion, "watch alerts", 0, false, "last reached watch alerts"},
		{"old binary", recoveryGoodVersion, lifecycleFirstPass, 0, true, "no matching candidate"},
		{"before install", recoveryBadVersion, lifecycleFirstPass, -time.Minute, true, "no matching candidate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, now := recoveryRig(t)
			if err := c.store.WriteFile(throughputObservationFile, throughputObservation{At: now.Add(-time.Minute), Candidate: recoveryGoodVersion}); err != nil {
				t.Fatal(err)
			}
			life := loopLifecycle{Version: tc.version, Started: now.Add(tc.age), At: *now, Phase: tc.phase, Exited: tc.exited, Code: 1}
			if err := c.store.WriteFile(loopLifecycleFile, life); err != nil {
				t.Fatal(err)
			}
			for range int(update.HeartbeatGrace/time.Minute) + 1 {
				c.mustTick(t)
			}
			j := journal(t, c)
			if !j.RolledBack() || !strings.Contains(j.Observed.Reason, tc.want) {
				t.Fatalf("lifecycle changed verdict or hid phase: %+v", j)
			}
		})
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
	if err := c.store.WriteFile(loopVersionFile, loopVersionRecord{Version: recoveryGoodVersion, PID: os.Getpid()}); err != nil {
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
	if err := c.store.WriteFile(loopLifecycleFile, loopLifecycle{Version: recoveryBadVersion, Started: *now, At: *now, Phase: lifecycleFirstPass}); err != nil {
		t.Fatal(err)
	}
	if err := c.store.WriteFile(loopVersionFile, loopVersionRecord{Version: recoveryBadVersion, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	for range int(update.HeartbeatGrace / time.Minute) {
		c.mustTick(t)
		if installed(t, c) != recoveryBadBytes {
			t.Fatalf("rolled back within the candidate loop's grace: %s", journal(t, c).Observed.Reason)
		}
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
	if err := c.store.WriteFile(loopVersionFile, loopVersionRecord{Version: recoveryGoodVersion, PID: os.Getpid()}); err != nil {
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

func TestLoopExitPersistsDiagnosticWithoutErrorContents(t *testing.T) {
	r := newLoopRig(t)
	r.failRecord = func(e state.Event) bool { return e.Verb == loopStartVerb }
	if err := r.a.runSupervisorLoop(t.Context(), true); err == nil {
		t.Fatal("expected injected event write failure")
	}
	var life loopLifecycle
	found, err := r.a.store.ReadFile(loopLifecycleFile, &life)
	if err != nil || !found || !life.Exited || life.Code != 1 || life.Phase != "startup" {
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
		if got := loopFailureKind(tc.err); got != tc.want {
			t.Fatalf("got %q want %q", got, tc.want)
		}
	}
}

func TestSecondLoopCannotReplaceOwnerLifecycle(t *testing.T) {
	r := newLoopRig(t)
	owner := loopLifecycle{Version: recoveryBadVersion, Started: time.Now(), Phase: "watch alerts"}
	if err := r.a.store.WriteFile(loopLifecycleFile, owner); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(filepath.Join(r.a.store.Dir(), loopLockFile))
	if held, err := lock.TryLock(); !held || err != nil {
		t.Fatalf("lock: %v %v", held, err)
	}
	defer func() { _ = lock.Unlock() }()
	if err := r.a.runSupervisorLoop(t.Context(), true); Code(err) != ExitRefused {
		t.Fatalf("second loop: %v", err)
	}
	var got loopLifecycle
	if _, err := r.a.store.ReadFile(loopLifecycleFile, &got); err != nil || got.Exited || got.Phase != owner.Phase || !got.Started.Equal(owner.Started) {
		t.Fatalf("contender overwrote owner: %+v %v", got, err)
	}
}

func TestRecoveryIdentifiesIncumbentThatNeverHandedOff(t *testing.T) {
	c, now := recoveryRig(t)
	if err := c.store.WriteFile(loopVersionFile, loopVersionRecord{PID: os.Getpid(), Version: recoveryGoodVersion}); err != nil {
		t.Fatal(err)
	}
	if got := c.loopSilence(recoveryBadVersion, *now); !strings.Contains(got, "another-version loop is still alive") {
		t.Fatalf("lost incumbent evidence: %s", got)
	}
}

func TestLoopSuccessfulExitHasZeroDiagnosticCode(t *testing.T) {
	r := newLoopRig(t)
	r.pass(t)
	var life loopLifecycle
	if found, err := r.a.store.ReadFile(loopLifecycleFile, &life); err != nil || !found || !life.Exited || life.Code != 0 || life.Failure != "" {
		t.Fatalf("successful loop recorded failure: %+v %v", life, err)
	}
}

func TestLoopPanicIsRecordedAndPropagated(t *testing.T) {
	r := newLoopRig(t)
	r.failRecord = func(e state.Event) bool {
		if e.Verb == loopStartVerb {
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
	var life loopLifecycle
	if found, err := r.a.store.ReadFile(loopLifecycleFile, &life); err != nil || !found || !life.Exited || life.Code != 1 || life.Failure != "panic (see candidate service journal)" {
		t.Fatalf("panic recorded incorrectly: %+v %v", life, err)
	}
}
