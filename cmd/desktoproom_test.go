package cmd

import (
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// At the desktop's cap beekeeper ends only one of its own finished CLIs,
// idle longest: never the person's own session, a role's holder, an agent
// busy with its task or a CLI it keeps (the steward of a send).
func TestRoomFor(t *testing.T) {
	now := time.Date(2026, 10, 5, 22, 0, 0, 0, time.UTC)
	desktop := []string{claudeComm, permissionPromptTool, "stdio"}
	session := func(pid int, id string, idle time.Duration) *claude.Session {
		return &claude.Session{PID: pid, ID: id, HostID: "local_" + id, LastActive: now.Add(-idle)}
	}
	// own is the person's own session, idle longest of all; fresh and
	// stale are beekeeper's finished workers; tasked has a task; watcher
	// supervises.
	sessions := []*claude.Session{
		session(1, "own", 90*time.Hour),
		session(2, "fresh", 2*time.Hour),
		session(3, "stale", 5*time.Hour),
		session(4, "tasked", 9*time.Hour),
		session(5, "watcher", 8*time.Hour),
	}
	tbl := &proc.Table{ByPID: map[int]*proc.Process{}}
	for _, s := range sessions {
		tbl.ByPID[s.PID] = &proc.Process{PID: s.PID, Comm: claudeComm, Args: desktop}
	}
	st := &state.State{}
	for _, id := range []string{"fresh", "stale", "tasked", "watcher"} {
		st.Starts = append(st.Starts, state.Start{Party: state.Party{Session: id, HostSession: "local_" + id}})
	}
	st.Agents = []state.Agent{{Party: state.Party{Session: "tasked"}, Task: "its own task"}}
	st.Supervisor = &state.Supervisor{Party: state.Party{Session: "watcher", HostSession: "local_watcher"}, Since: now.Add(-time.Hour)}
	const stale, fresh = "local_stale", "local_fresh"
	if got := roomFor(st, sessions, tbl, now, nil); got == nil || got.HostID != stale {
		t.Fatalf("ends %v, want beekeeper's finished worker idle longest, %s", got, stale)
	}
	if got := roomFor(st, sessions, tbl, now, []string{stale}); got == nil || got.HostID != fresh {
		t.Fatalf("keeping %s ends %v, want %s", stale, got, fresh)
	}
	if got := roomFor(st, sessions, tbl, now, []string{stale, fresh}); got != nil {
		t.Fatalf("with no finished worker of its own it ends %s, want none", got.HostID)
	}
	if n := desktopCLIs(tbl); n != len(sessions) {
		t.Errorf("counts %d desktop CLIs, want %d", n, len(sessions))
	}
}
