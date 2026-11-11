package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecideBoundaries(t *testing.T) {
	loop := Signals{HeartbeatRequired: true, Heartbeat: true}
	silent := Signals{HeartbeatRequired: true}
	for _, tc := range []struct {
		name string
		o    Observed
		s    Signals
		want string
	}{
		{"a failure streak rolls back at once", Observed{Elapsed: time.Minute}, Signals{Failure: "failed-merge-exit-1"}, RollBack},
		{"two failed probes wait", Observed{ProbeFailures: ProbeFailures - 1}, Signals{}, Wait},
		{"three failed probes roll back", Observed{ProbeFailures: ProbeFailures}, Signals{}, RollBack},
		{"a silent loop within its grace waits", Observed{Elapsed: HeartbeatGrace - time.Second}, silent, Wait},
		{"a silent loop at its grace rolls back", Observed{Elapsed: HeartbeatGrace}, silent, RollBack},
		{"a loop not yet handed over waits past the grace", Observed{Elapsed: HeartbeatGrace}, Signals{HeartbeatRequired: true, LoopPending: true}, Wait},
		{"a loop never handed over rolls back at the window", Observed{Elapsed: ProbationWindow}, Signals{HeartbeatRequired: true, LoopPending: true}, RollBack},
		{"a late loop within its own grace waits", Observed{Elapsed: HeartbeatGrace + 5*time.Minute, LoopStarted: true, LoopStartedAt: 8 * time.Minute}, silent, Wait},
		{"a late loop at its own grace rolls back", Observed{Elapsed: HeartbeatGrace + 8*time.Minute, LoopStarted: true, LoopStartedAt: 8 * time.Minute}, silent, RollBack},
		{"progress renews grace after startup", Observed{Elapsed: 15 * time.Minute, LoopStarted: true, HeartbeatSeen: true, LastBeatAt: 11 * time.Minute}, silent, Wait},
		{"latest checkpoint grace expires", Observed{Elapsed: 21 * time.Minute, LoopStarted: true, HeartbeatSeen: true, LastBeatAt: 11 * time.Minute}, silent, RollBack},
		{"stale heartbeat cannot promote at window", Observed{Elapsed: ProbationWindow, ProbePassed: true, HeartbeatSeen: true, LastBeatAt: 26 * time.Minute}, silent, Wait},
		{"a host owes no heartbeat", Observed{Elapsed: HeartbeatGrace, ProbePassed: true}, Signals{}, Wait},
		{"one second short of the window waits", Observed{Elapsed: ProbationWindow - time.Second, ProbePassed: true}, loop, Wait},
		{"the window end promotes", Observed{Elapsed: ProbationWindow, ProbePassed: true}, loop, Promote},
		{"no probe, no promotion", Observed{Elapsed: 2 * ProbationWindow}, loop, Wait},
		{"an outage keeps the candidate unproven", Observed{Elapsed: 2 * ProbationWindow, ProbeFailures: ProbeFailures}, Signals{Unproven: "GitHub unavailable"}, Wait},
		{"a stop counts for nothing", Observed{Elapsed: 2 * ProbationWindow}, Signals{Intentional: true, Failure: "failed-merge-exit-1"}, Wait},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, why := Decide(tc.o, tc.s); got != tc.want {
				t.Fatalf("%s (%s), want %s", got, why, tc.want)
			}
		})
	}
}

func TestNoteLoopKeepsTheFirstStartInObservedTime(t *testing.T) {
	o := Observed{Elapsed: 9 * time.Minute}
	o.NoteLoop(Signals{})
	if o.LoopStarted {
		t.Fatal("noted a loop that did not start")
	}
	// A start longer ago than observed time (a machine that slept) counts from 0.
	o.NoteLoop(Signals{LoopStarted: true, LoopStartedAgo: time.Hour})
	if !o.LoopStarted || o.LoopStartedAt != 0 {
		t.Fatalf("%+v", o)
	}
	o = Observed{Elapsed: 9 * time.Minute}
	o.NoteLoop(Signals{LoopStarted: true, LoopStartedAgo: time.Minute})
	o.Elapsed += time.Minute
	o.NoteLoop(Signals{LoopStarted: true})
	if o.LoopStartedAt != 8*time.Minute {
		t.Fatalf("start moved: %+v", o)
	}
}

func TestNoteLoopTracksCheckpointAgeWithoutRenewingAnOldBeat(t *testing.T) {
	o := Observed{Elapsed: 12 * time.Minute, LoopStarted: true}
	o.NoteLoop(Signals{Heartbeat: true, HeartbeatAgo: time.Minute})
	if !o.HeartbeatSeen || o.LastBeatAt != 11*time.Minute {
		t.Fatalf("lost checkpoint age: %+v", o)
	}
	// Re-reading the same checkpoint two minutes later must not buy more time.
	o.Elapsed += 2 * time.Minute
	o.NoteLoop(Signals{Heartbeat: true, HeartbeatAgo: 3 * time.Minute})
	if o.LastBeatAt != 11*time.Minute {
		t.Fatalf("renewed an old checkpoint: %+v", o)
	}
	o.NoteLoop(Signals{})
	if o.LastBeatAt != 11*time.Minute {
		t.Fatalf("silence changed heartbeat history: %+v", o)
	}
}

func TestInspectCreatesNothing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	target := filepath.Join(dir, "beekeeper")
	if err := os.WriteFile(target, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	j, err := Inspect(target)
	if err != nil || j != nil {
		t.Fatalf("%v, %v", j, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("%v, %v", entries, err)
	}
	if WatchedProbation(target) || Quarantined(target, stable) != "" {
		t.Fatal("an installation without a journal has no probation")
	}
}

const watchedCandidate = "v1.1.0-rc.2"

// watchedFixture has a supervised candidate, as a launched controller leaves it.
func watchedFixture(t *testing.T) (*installation, *Journal) {
	t.Helper()
	i, dir := recoveryFixture(t)
	old := stageRecovery(t, dir, "old")
	if err := i.replace(old, stable); err != nil {
		t.Fatal(err)
	}
	j, err := i.read()
	if err != nil {
		t.Fatal(err)
	}
	if err := i.promote(j); err != nil {
		t.Fatal(err)
	}
	launched := Binary{}
	i.ctx = WithOptions(i.ctx, Options{Recovery: func(_ context.Context, previous Binary, artifact, target string) error {
		launched = previous
		if _, err := os.Stat(artifact); err != nil {
			t.Fatal(err)
		}
		if got, err := digest(target); err != nil || got != previous.Digest {
			t.Fatalf("launched after the replacement: %s, %v", got, err)
		}
		return nil
	}})
	if err := i.replace(stageRecovery(t, dir, "candidate"), watchedCandidate); err != nil {
		t.Fatal(err)
	}
	if j, err = i.read(); err != nil {
		t.Fatal(err)
	}
	if !j.Watched || launched.Version != stable {
		t.Fatalf("watched %v, launched %+v", j.Watched, launched)
	}
	return i, j
}

func TestWatchedCandidateWaitsForItsVerdict(t *testing.T) {
	i, j := watchedFixture(t)
	prev := currentVersion
	t.Cleanup(func() { currentVersion = prev })
	currentVersion = func() string { return j.Candidate.Version }
	if err := i.settleProbation(context.Background(), nil); !errors.Is(err, ErrProbationWatched) {
		t.Fatalf("an update promoted a supervised candidate: %v", err)
	}
	if !WatchedProbation(i.target) {
		t.Fatal("the loop would ask for an update it is refused")
	}
}

func TestWatchedSupervisionIsDurableBeforeReplacement(t *testing.T) {
	i, dir := recoveryFixture(t)
	if err := i.replace(stageRecovery(t, dir, "old"), stable); err != nil {
		t.Fatal(err)
	}
	j, err := i.read()
	if err != nil {
		t.Fatal(err)
	}
	if err := i.promote(j); err != nil {
		t.Fatal(err)
	}
	i.ctx = WithOptions(i.ctx, Options{Recovery: func(context.Context, Binary, string, string) error { return nil }})
	original := journalRename
	swapped := false
	journalRename = func(from, to string) error {
		if swapped {
			return errors.New("crash after swap")
		}
		if to == i.target {
			swapped = true
		}
		return original(from, to)
	}
	t.Cleanup(func() { journalRename = original })
	if err := i.replace(stageRecovery(t, dir, "candidate"), watchedCandidate); err == nil {
		t.Fatal("expected post-swap journal write failure")
	}
	journalRename = original
	assertRecoveryBytes(t, i.target, "candidate")
	j = readRecovery(t, i)
	if j.Probation != probationPrepared || !j.Watched {
		t.Fatalf("supervision lost before replacement: %+v", j)
	}
	if !WatchedProbation(i.target) {
		t.Fatal("rearmRecovery would refuse to launch controller before reconciliation")
	}
	prev := currentVersion
	t.Cleanup(func() { currentVersion = prev })
	currentVersion = func() string { return j.Candidate.Version }
	if err := i.settleProbation(context.Background(), nil); !errors.Is(err, ErrProbationWatched) {
		t.Fatalf("unsupervised promotion allowed after post-swap crash: %v", err)
	}
	j = readRecovery(t, i)
	if j.Probation != probationPending || !j.Watched {
		t.Fatalf("reconciled journal lost supervision: %+v", j)
	}
}

func TestUnlaunchedControllerLeavesTheCandidateUnwatched(t *testing.T) {
	i, dir := recoveryFixture(t)
	if err := i.replace(stageRecovery(t, dir, "old"), stable); err != nil {
		t.Fatal(err)
	}
	j, _ := i.read()
	if err := i.promote(j); err != nil {
		t.Fatal(err)
	}
	i.ctx = WithOptions(i.ctx, Options{Recovery: func(context.Context, Binary, string, string) error { return errors.New("no launcher") }})
	if err := i.replace(stageRecovery(t, dir, "candidate"), "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	if j, _ = i.read(); j.Watched {
		t.Fatal("no controller runs for this candidate")
	}
}

// A release candidate is what desks install, and what must roll back.
func TestPrereleaseCandidateRollsBack(t *testing.T) {
	i, j := watchedFixture(t)
	i.close()
	guard := func(previous, failed string, restore func() error) error {
		if previous != stable || failed != watchedCandidate {
			t.Fatalf("%s, %s", previous, failed)
		}
		return restore()
	}
	if err := Rollback(context.Background(), i.target, "failed probe", guard); err != nil {
		t.Fatal(err)
	}
	got, err := digest(i.target)
	if err != nil || got != j.Previous.Digest {
		t.Fatalf("%s, %v", got, err)
	}
	if why := Quarantined(i.target, "1.1.0-rc.2"); why != "failed probe" {
		t.Fatalf("quarantine %q", why)
	}
}

func TestRecoveryHoldRefusesUpdatesUntilCleared(t *testing.T) {
	i, j := watchedFixture(t)
	i.close()
	ctx := context.Background()
	if err := Observe(ctx, i.target, func(j *Journal) error {
		j.Observed = &Observed{Held: "corrupt recovery target"}
		j.Quarantine = map[string]string{j.Candidate.Digest: "broken"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	next, err := openInstallation(ctx, i.target)
	if err != nil {
		t.Fatal(err)
	}
	defer next.close()
	staged := stageRecovery(t, filepath.Dir(i.target), "another")
	if err := next.replace(staged, "v1.2.0"); err == nil || !strings.Contains(err.Error(), "updates are held") {
		t.Fatalf("a held machine updated: %v", err)
	}
	next.close()
	if err := ClearHold(ctx, i.target); err != nil {
		t.Fatal(err)
	}
	after, err := Inspect(i.target)
	if err != nil || after.Held() != "" || after.Quarantine[j.Candidate.Digest] == "" {
		t.Fatalf("%+v, %v", after, err)
	}
}

func TestSettled(t *testing.T) {
	for _, tc := range []struct {
		name string
		j    *Journal
		want bool
	}{
		{"none", nil, true},
		{"pending", &Journal{Probation: probationPending}, false},
		{"passed", &Journal{Probation: probationPassed}, true},
		{"held", &Journal{Probation: probationPending, Observed: &Observed{Held: "x"}}, true},
		{"rolled back by hand", &Journal{Probation: probationRolledBack}, true},
		{"rolled back, unnotified", &Journal{Probation: probationRolledBack, Observed: &Observed{Restarted: true}}, false},
		{"rolled back, processes left", &Journal{Probation: probationRolledBack, Observed: &Observed{Notified: true}}, false},
		{"rolled back and done", &Journal{Probation: probationRolledBack, Observed: &Observed{Notified: true, Restarted: true}}, true},
	} {
		if got := tc.j.Settled(); got != tc.want {
			t.Fatalf("%s: %v", tc.name, got)
		}
	}
}
