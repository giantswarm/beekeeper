package codex

import (
	"fmt"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

func TestDiscoverReservesExplicitThreads(t *testing.T) {
	for _, resumedFirst := range []bool{true, false} {
		for _, haveNewRollout := range []bool{false, true} {
			t.Run(fmt.Sprintf("resumed-first=%t/new-rollout=%t", resumedFirst, haveNewRollout), func(t *testing.T) {
				testDiscoverReservedThread(t, resumedFirst, haveNewRollout)
			})
		}
	}
}

func testDiscoverReservedThread(t *testing.T, resumedFirst, haveNewRollout bool) {
	t.Helper()
	now := time.Now()
	stateDir := t.TempDir()
	// The synthetic processes have no live /proc entries, so both cwd reads
	// return empty. This models two processes in the same working directory.
	path := writeRollout(t, HomeSessionsDir(stateDir), "rollout-resumed.jsonl", now,
		`{"type":"session_meta","payload":{"id":"resumed-thread","timestamp":"2026-10-01T10:00:00Z","cwd":""}}`)
	resumed := &proc.Process{PID: 1000000000, Comm: Comm, Args: []string{Comm, "exec", "resume", "resumed-thread", "-"}, Start: now.Add(-time.Minute)}
	starting := &proc.Process{PID: 1000000001, Comm: Comm, Args: []string{Comm, "exec", "-"}, Start: now.Add(-time.Second)}
	if !resumedFirst {
		resumed.PID, starting.PID = starting.PID, resumed.PID
	}
	var newPath, newID string
	if haveNewRollout {
		newID = "new-thread"
		newPath = writeRollout(t, HomeSessionsDir(stateDir), "rollout-new.jsonl", now,
			`{"type":"session_meta","payload":{"id":"new-thread","cwd":""}}`)
	}
	sessions := Discover(stateDir, "", proc.NewTable(resumed, starting), now)
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(sessions))
	}
	for _, s := range sessions {
		if s.PID == resumed.PID && (s.Transcript != path || s.ID != "resumed-thread") {
			t.Fatalf("resumed session lost its explicit transcript: %+v", s)
		}
		if s.PID == starting.PID && (s.Transcript != newPath || s.ID != newID) {
			t.Fatalf("new session has another process's transcript: %+v", s)
		}
	}
}
