package cmd

import (
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// desktopArgs are the arguments of a CLI the desktop runs.
var desktopArgs = []string{claudeComm, permissionPromptTool, "stdio"}

// At the desktop's cap beekeeper ends only one of its own idle CLIs: a
// finished worker's first, then a parked one's, idle longest first; never
// the person's own session, a role's holder, a worker busy on its task, one
// whose merge waits in the gate, or a CLI it keeps (the steward of a send).
func TestRoomFor(t *testing.T) {
	now := time.Date(2026, 10, 5, 22, 0, 0, 0, time.UTC)
	const onhold = "onhold"
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
		session(6, onhold, time.Hour),
	}
	tbl := &proc.Table{ByPID: map[int]*proc.Process{}}
	for _, s := range sessions {
		tbl.ByPID[s.PID] = &proc.Process{PID: s.PID, Comm: claudeComm, Args: desktopArgs}
	}
	st := &state.State{}
	for _, id := range []string{"fresh", "stale", "tasked", "watcher", onhold} {
		st.Starts = append(st.Starts, state.Start{Party: state.Party{Session: id, HostSession: "local_" + id}})
	}
	st.Agents = []state.Agent{
		{Party: state.Party{Session: "tasked"}, Task: "its own task"},
		{Party: state.Party{Session: onhold}, Task: "its parked task", Park: &state.Park{}},
	}
	st.Supervisor = &state.Supervisor{Party: state.Party{Session: "watcher", HostSession: "local_watcher"}, Since: now.Add(-time.Hour)}
	const stale, fresh, waiting = "local_stale", "local_fresh", "local_onhold"
	if got := roomFor(st, sessions, tbl, now, nil, false); got == nil || got.HostID != stale {
		t.Fatalf("ends %v, want beekeeper's finished worker idle longest, %s", got, stale)
	}
	if got := roomFor(st, sessions, tbl, now, []string{stale}, false); got == nil || got.HostID != fresh {
		t.Fatalf("keeping %s ends %v, want %s", stale, got, fresh)
	}
	if got := roomFor(st, sessions, tbl, now, []string{stale, fresh}, false); got == nil || got.HostID != waiting {
		t.Fatalf("with no finished worker it ends %v, want the parked one", got)
	}
	if got := roomFor(st, sessions, tbl, now, []string{stale, fresh, waiting}, false); got != nil {
		t.Fatalf("with only the person's own session, a role's holder and a busy worker left it ends %s, want none", got.HostID)
	}
	st.Merges = []state.Merge{{Repo: "o/r", PR: 1, By: state.Party{Session: onhold}}}
	if got := roomFor(st, sessions, tbl, now, []string{stale, fresh}, false); got != nil {
		t.Fatalf("a parked worker whose merge waits in the gate is ended: %s", got.HostID)
	}
	// For a role's run the worker idle on its task makes room last.
	st.Merges = nil
	if got := roomFor(st, sessions, tbl, now, []string{stale, fresh, waiting}, true); got == nil || got.HostID != "local_tasked" {
		t.Fatalf("for a role's run it ends %v, want the worker idle on its task", got)
	}
	if got := roomFor(st, sessions, tbl, now, []string{stale, fresh}, true); got == nil || got.HostID != waiting {
		t.Fatalf("for a role's run it ends %v before the parked one, want %s", got, waiting)
	}
	if n := desktopCLIs(tbl); n != len(sessions) {
		t.Errorf("counts %d desktop CLIs, want %d", n, len(sessions))
	}
}

// A relayed role run's successor gets its desktop CLI at the cap: the run the
// relay relieved is past (its work ended with the relay), so its idle CLI
// makes room, and the forRole successor may end a worker idle on its task.
// Neither the holder nor a run still active within stewardQuiet is ended.
func TestRoomForRelievedRun(t *testing.T) {
	now := time.Date(2026, 10, 6, 13, 4, 0, 0, time.UTC)
	old := state.Party{Session: "pastrun", HostSession: "local_pastrun", Name: "Guide run 12"}
	succ := state.Party{Session: "successor", HostSession: "local_successor", Name: "Guide run 13"}
	sessions := []*claude.Session{
		{PID: 1, ID: old.Session, HostID: old.HostSession, Name: old.Name, LastActive: now.Add(-3 * time.Minute)},
		{PID: 2, ID: succ.Session, HostID: succ.HostSession, Name: succ.Name, LastActive: now.Add(-2 * time.Minute)},
	}
	tbl := &proc.Table{ByPID: map[int]*proc.Process{}}
	for _, s := range sessions {
		tbl.ByPID[s.PID] = &proc.Process{PID: s.PID, Comm: claudeComm, Args: desktopArgs}
	}
	st := &state.State{Starts: []state.Start{{Party: old}, {Party: succ}}}
	guideRole.update(st, func(r *state.Role) {
		r.Holder = &state.Supervisor{Party: succ, Since: now.Add(-3 * time.Minute)}
		r.Relay = &state.Relay{From: old, To: succ, At: now.Add(-3 * time.Minute), Taken: now.Add(-3 * time.Minute), Expires: now.Add(time.Hour)}
		r.Relieved = []state.Relief{{Party: old, Taken: now.Add(-3 * time.Minute)}}
	})
	keep := []string{succ.HostSession}
	if !forRole(st, keep, now) {
		t.Fatal("the role's holder is no role's run")
	}
	if got := roomFor(st, sessions, tbl, now, keep, true); got == nil || got.HostID != old.HostSession {
		t.Fatalf("ends %v, want the relieved run's CLI %s", got, old.HostSession)
	}
	if got := roomFor(st, sessions, tbl, now, nil, false); got == nil || got.HostID != old.HostSession {
		t.Fatalf("ends %v, want the relieved run's CLI %s, never the holder's", got, old.HostSession)
	}
	sessions[0].LastActive = now.Add(-10 * time.Second)
	if got := roomFor(st, sessions, tbl, now, keep, true); got != nil {
		t.Fatalf("ends %s, a relieved run still writing its hand-over", got.HostID)
	}
	if forRole(st, []string{"local_worker"}, now) {
		t.Fatal("a worker counts as a role's run")
	}
}
