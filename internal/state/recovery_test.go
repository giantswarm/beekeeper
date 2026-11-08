package state

import (
	"errors"
	"os"
	"testing"
	"time"
)

const (
	recoveryRelease   = "v1.0.0"
	recoveryDevBuild  = "dev"
	recoveryNewer     = "v2.0.0"
	recoveryCandidate = "v2.0.0-rc.1"
	recoveryFailed    = "v3.0.0"
)

func TestRecoverVersionFencesLiveStateAndPreservesEveryByte(t *testing.T) {
	for _, tc := range []struct {
		name, writer, target string
		allowed              bool
	}{
		{"upgrade", recoveryRelease, recoveryNewer, true},
		{"same release", recoveryNewer, recoveryNewer, true},
		{"downgrade after candidate writes", recoveryNewer, recoveryRelease, false},
		{"downgrade before candidate writes", recoveryRelease, recoveryRelease, true},
		{"unknown target", recoveryRelease, recoveryDevBuild, false},
		{"prerelease target past the writer", recoveryRelease, recoveryCandidate, true},
		{"prerelease target behind the writer", recoveryNewer, recoveryCandidate, false},
		{"unknown writer", recoveryDevBuild, recoveryRelease, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := OpenVersion(dir, recoveryNewer)
			if err != nil {
				t.Fatal(err)
			}
			// Future fields and all existing work/approval/note data must survive intact.
			raw := []byte(`{"writer":{"version":"` + tc.writer + `"},"notes":[{"id":1}],"futureWork":{"task":"keep"},"futureApprovals":["keep"]}`)
			if err := os.WriteFile(s.path("state.json"), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			events := []byte("post-install events\n")
			if err := os.WriteFile(s.path("events.jsonl"), events, 0o600); err != nil {
				t.Fatal(err)
			}
			called := false
			err = s.RecoverVersion(tc.target, recoveryFailed, func() error {
				called = true
				// Another writer cannot slip between the compatibility check and restore.
				other, _ := Open(dir)
				err := other.UpdateWithin(time.Millisecond, func(*State) ([]Event, error) { t.Error("concurrent write ran"); return nil, nil })
				var busy *LockBusyError
				if !errors.As(err, &busy) {
					t.Errorf("state lock = %v", err)
				}
				return nil
			})
			if (err == nil) != tc.allowed || called != tc.allowed {
				t.Fatalf("allowed=%v called=%v error=%v", tc.allowed, called, err)
			}
			for name, want := range map[string][]byte{"state.json": raw, "events.jsonl": events} {
				got, err := os.ReadFile(s.path(name))
				if err != nil || string(got) != string(want) {
					t.Fatalf("%s changed: %s %v", name, got, err)
				}
			}
		})
	}
}

func TestRecoveredWriterCeilingSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	old, _ := OpenVersion(dir, recoveryRelease)
	candidate, _ := OpenVersion(dir, recoveryNewer)
	noop := func(*State) ([]Event, error) { return nil, nil }
	if err := old.Update(noop); err != nil {
		t.Fatal(err)
	}
	if err := candidate.RecoverVersion(recoveryRelease, recoveryNewer, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{recoveryNewer, recoveryCandidate, recoveryDevBuild} {
		restarted, _ := OpenVersion(dir, v)
		if err := restarted.Update(noop); err == nil {
			t.Fatalf("%s advanced recovered state", v)
		}
	}
	if err := old.Update(noop); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Record(Event{Verb: "after-recovery"}); err != nil {
		t.Fatal(err)
	}
	if err := candidate.ResumeRecoveredWrites(recoveryNewer); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Update(noop); err != nil {
		t.Fatal(err)
	}
	if err := old.Update(noop); err == nil {
		t.Fatal("old writer bypassed normal stale-writer fence")
	}
	events, err := candidate.Events(0, nil)
	if err != nil || len(events) < 1 || events[0].Verb != "after-recovery" {
		t.Fatalf("events %+v %v", events, err)
	}
}

func TestRecoveryRefusesExistingUnversionedState(t *testing.T) {
	s, _ := Open(t.TempDir())
	if err := os.WriteFile(s.path("state.json"), []byte(`{"notes":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverVersion(recoveryRelease, recoveryNewer, func() error { t.Fatal("recovered unknown state"); return nil }); err == nil {
		t.Fatal("accepted unversioned state")
	}
}
func TestRecoveryWithNoStateFile(t *testing.T) {
	s, _ := Open(t.TempDir())
	called := false
	if err := s.RecoverVersion(recoveryRelease, recoveryNewer, func() error { called = true; return nil }); err != nil || !called {
		t.Fatalf("called %v error %v", called, err)
	}
	if _, err := os.Stat(s.path("state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created state during recovery: %v", err)
	}
}

// A prerelease stamps nothing: state left by one, unversioned, does not stop
// the rollback of a release candidate, the release the desks follow.
func TestRecoveryOfAPrereleaseFromUnversionedState(t *testing.T) {
	dir := t.TempDir()
	noop := func(*State) ([]Event, error) { return nil, nil }
	s, err := OpenVersion(dir, "v2.0.0-rc.2")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(noop); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := s.RecoverVersion(recoveryCandidate, "v2.0.0-rc.2", func() error { called = true; return nil }); err != nil || !called {
		t.Fatalf("called %v: %v", called, err)
	}
	if err := s.Update(noop); err == nil {
		t.Fatal("the failed candidate wrote past the fence")
	}
	restored, _ := OpenVersion(dir, recoveryCandidate)
	if err := restored.Update(noop); err != nil {
		t.Fatal(err)
	}
}

// The fence stops the failed release's processes, never a later release: that
// one could not write, so it could never pass its probation.
func TestRecoveryFenceLetsALaterReleaseWrite(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenVersion(dir, recoveryRelease)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverVersion(recoveryRelease, recoveryNewer, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	noop := func(*State) ([]Event, error) { return nil, nil }
	// The later release comes last: its save stamps the writer, which older
	// versions then fail on for that reason instead of the fence.
	for _, tc := range []struct {
		version string
		allowed bool
	}{{recoveryRelease, true}, {"v1.5.0", false}, {recoveryNewer, false}, {recoveryDevBuild, false}, {"v2.0.1", true}} {
		w, _ := OpenVersion(dir, tc.version)
		if err := w.Update(noop); (err == nil) != tc.allowed {
			t.Fatalf("%s: allowed %v, got %v", tc.version, tc.allowed, err)
		}
	}
}

func TestCompleteRollbackResumesCompatibleWritersWithoutUnfencingFailedBinary(t *testing.T) {
	dir := t.TempDir()
	restored, _ := OpenVersion(dir, recoveryRelease)
	noop := func(*State) ([]Event, error) { return nil, nil }
	if err := restored.Update(noop); err != nil {
		t.Fatal(err)
	}
	if err := restored.RecoverVersion(recoveryRelease, recoveryNewer, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := restored.CompleteRollback(recoveryRelease, recoveryNewer); err != nil {
		t.Fatal(err)
	}
	// Restarted services on a compatible intermediate version need no manual
	// probation completion. Neither failed writers nor unknown builds resume.
	for _, tc := range []struct {
		version string
		allowed bool
	}{{recoveryRelease, true}, {recoveryCandidate, true}, {recoveryNewer, false}, {recoveryDevBuild, false}} {
		writer, _ := OpenVersion(dir, tc.version)
		if err := writer.Update(noop); (err == nil) != tc.allowed {
			t.Fatalf("%s: allowed %v, got %v", tc.version, tc.allowed, err)
		}
	}
	var fence recoveryFence
	if _, err := restored.ReadFile(recoveryFenceFile, &fence); err != nil {
		t.Fatal(err)
	}
	// Preserve the fields understood by the rolled-back binary. Removing the
	// file would allow its still-running processes to write again.
	if !fence.Recovered || fence.Maximum != recoveryRelease || fence.Failed != recoveryNewer {
		t.Fatalf("lost legacy process fence: %+v", fence)
	}
	if err := restored.CompleteRollback(recoveryRelease, recoveryNewer); err != nil {
		t.Fatalf("completion not idempotent: %v", err)
	}
	if err := restored.CompleteRollback(recoveryRelease, recoveryFailed); err == nil {
		t.Fatal("completed a different rollback")
	}
}

func TestCompleteRollbackRefusesIncompatibleLiveWriter(t *testing.T) {
	s, _ := OpenVersion(t.TempDir(), recoveryRelease)
	if err := s.RecoverVersion(recoveryRelease, recoveryNewer, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path("state.json"), []byte(`{"writer":{"version":"`+recoveryNewer+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteRollback(recoveryRelease, recoveryNewer); err == nil {
		t.Fatal("lifted the ceiling over an incompatible writer")
	}
}
