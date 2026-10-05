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
	desktop := []string{claudeComm, "--output-format", "stream-json", permissionPromptTool, "stdio"}
	session := func(pid int, id string, idle time.Duration) *claude.Session {
		return &claude.Session{PID: pid, ID: id, HostID: "local_" + id, LastActive: now.Add(-idle)}
	}
	// personal is the person's own session, idle longest of all; done and
	// older are beekeeper's finished workers; busy has a task; holder
	// supervises.
	sessions := []*claude.Session{
		session(1, "personal", 90*time.Hour),
		session(2, "done", 2*time.Hour),
		session(3, "older", 5*time.Hour),
		session(4, "busy", 9*time.Hour),
		session(5, "holder", 8*time.Hour),
	}
	tbl := &proc.Table{ByPID: map[int]*proc.Process{}}
	for _, s := range sessions {
		tbl.ByPID[s.PID] = &proc.Process{PID: s.PID, Comm: claudeComm, Args: desktop}
	}
	st := &state.State{}
	for _, id := range []string{"done", "older", "busy", "holder"} {
		st.Starts = append(st.Starts, state.Start{Party: state.Party{Session: id, HostSession: "local_" + id}})
	}
	st.Agents = []state.Agent{{Party: state.Party{Session: "busy"}, Task: "work"}}
	st.Supervisor = &state.Supervisor{Party: state.Party{Session: "holder", HostSession: "local_holder"}, Since: now.Add(-time.Hour)}
	if got := roomFor(st, sessions, tbl, now, nil); got == nil || got.HostID != "local_older" {
		t.Fatalf("ends %v, want beekeeper's finished worker idle longest, local_older", got)
	}
	if got := roomFor(st, sessions, tbl, now, []string{"local_older"}); got == nil || got.HostID != "local_done" {
		t.Fatalf("keeping local_older ends %v, want local_done", got)
	}
	if got := roomFor(st, sessions, tbl, now, []string{"local_older", "local_done"}); got != nil {
		t.Fatalf("with no finished worker of its own it ends %s, want none", got.HostID)
	}
	if n := desktopCLIs(tbl); n != len(sessions) {
		t.Errorf("counts %d desktop CLIs, want %d", n, len(sessions))
	}
}
