package cmd

import (
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// At the desktop's cap beekeeper ends only one of its own idle CLIs: a
// finished worker's first, then a parked one's, then one idle on its task,
// idle longest first; never the person's own session, a role's holder or a
// CLI it keeps (the steward of a send).
func TestRoomFor(t *testing.T) {
	now := time.Date(2026, 10, 5, 22, 0, 0, 0, time.UTC)
	desktop := []string{claudeComm, permissionPromptTool, "stdio"}
	session := func(pid int, id string, idle time.Duration) *claude.Session {
		return &claude.Session{PID: pid, ID: id, HostID: "local_" + id, LastActive: now.Add(-idle)}
	}
	// own is the person's own session, idle longest of all; fresh and
	// stale are beekeeper's finished workers; onhold is parked on its person;
	// tasked is idle on its task; watcher supervises.
	sessions := []*claude.Session{
		session(1, "own", 90*time.Hour),
		session(2, "fresh", 2*time.Hour),
		session(3, "stale", 5*time.Hour),
		session(4, "tasked", 9*time.Hour),
		session(5, "watcher", 8*time.Hour),
		session(6, "onhold", time.Hour),
	}
	tbl := &proc.Table{ByPID: map[int]*proc.Process{}}
	for _, s := range sessions {
		tbl.ByPID[s.PID] = &proc.Process{PID: s.PID, Comm: claudeComm, Args: desktop}
	}
	st := &state.State{}
	for _, id := range []string{"fresh", "stale", "tasked", "watcher", "onhold"} {
		st.Starts = append(st.Starts, state.Start{Party: state.Party{Session: id, HostSession: "local_" + id}})
	}
	st.Agents = []state.Agent{
		{Party: state.Party{Session: "tasked"}, Task: "its own task"},
		{Party: state.Party{Session: "onhold"}, Task: "its parked task", Park: &state.Park{}},
	}
	st.Supervisor = &state.Supervisor{Party: state.Party{Session: "watcher", HostSession: "local_watcher"}, Since: now.Add(-time.Hour)}
	const stale, fresh, waiting = "local_stale", "local_fresh", "local_onhold"
	if got := roomFor(st, sessions, tbl, now, nil); got == nil || got.HostID != stale {
		t.Fatalf("ends %v, want beekeeper's finished worker idle longest, %s", got, stale)
	}
	if got := roomFor(st, sessions, tbl, now, []string{stale}); got == nil || got.HostID != fresh {
		t.Fatalf("keeping %s ends %v, want %s", stale, got, fresh)
	}
	if got := roomFor(st, sessions, tbl, now, []string{stale, fresh}); got == nil || got.HostID != waiting {
		t.Fatalf("with no finished worker it ends %v, want the parked one", got)
	}
	if got := roomFor(st, sessions, tbl, now, []string{stale, fresh, waiting}); got == nil || got.HostID != "local_tasked" {
		t.Fatalf("with no finished or parked worker it ends %v, want the one idle on its task", got)
	}
	if got := roomFor(st, sessions, tbl, now, []string{stale, fresh, waiting, "local_tasked"}); got != nil {
		t.Fatalf("with only the person's own session and a role's holder left it ends %s, want none", got.HostID)
	}
	if n := desktopCLIs(tbl); n != len(sessions) {
		t.Errorf("counts %d desktop CLIs, want %d", n, len(sessions))
	}
}
