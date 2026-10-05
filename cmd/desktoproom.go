package cmd

import (
	"context"
	"fmt"
	"os"
	"slices"
	"syscall"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// desktopCLICap is the desktop's cap of CLIs when its log names none: the
// governor's.
const desktopCLICap = 28

// roomWait bounds how long an ended CLI takes to exit.
const roomWait = 10 * time.Second

// desktopCLIs counts the CLIs the desktop runs: its governor's count.
func desktopCLIs(t *proc.Table) int {
	n := 0
	for _, p := range t.ByPID {
		if p.Comm == claudeComm && slices.Contains(p.Args, permissionPromptTool) {
			n++
		}
	}
	return n
}

// makeRoom keeps the desktop under its cap of CLIs before beekeeper makes
// it start one (an import, a show, a send that starts a turn). At its cap
// the desktop's governor pauses the CLI idle longest to start another,
// whoever's it is, the person's own sessions included; so at the cap
// beekeeper ends one of its own CLIs first (roomFor: a finished worker's,
// then a parked one's, then one idle on its task, never a role's), never
// one of keep (local_ ids). With none to end it starts nothing: the error
// names the cap.
func (a *app) makeRoom(ctx context.Context, keep ...string) error {
	return a.makeRoomFor(ctx, 1, keep...)
}

// makeRoomFor is makeRoom for slots CLIs the desktop is to start.
func (a *app) makeRoomFor(ctx context.Context, slots int, keep ...string) error {
	for {
		ended, err := a.endOneForRoom(ctx, slots, keep)
		if err != nil || !ended {
			return err
		}
	}
}

// endOneForRoom ends one of beekeeper's finished desktop CLIs when the
// desktop has fewer than slots free under its cap, and reports whether it
// ended one.
func (a *app) endOneForRoom(ctx context.Context, slots int, keep []string) (bool, error) {
	limit := desktopCLICap
	if n, ok := claude.DesktopCap(a.cfg.Claude.DesktopLog); ok {
		limit = n
	}
	st, err := a.store.Read()
	if err != nil {
		return false, err
	}
	sessions, t, err := a.sessions()
	if err != nil {
		return false, err
	}
	if desktopCLIs(t)+slots <= limit {
		return false, nil
	}
	s := roomFor(st, sessions, t, a.now, keep)
	if s == nil {
		return false, fmt.Errorf("the desktop runs its cap of %d CLIs and beekeeper runs no idle CLI of its own to end: it starts none, "+
			"since at the cap the desktop pauses the CLI idle longest, the person's own sessions included", limit)
	}
	pr, err := os.FindProcess(s.PID)
	if err == nil {
		err = pr.Signal(syscall.SIGTERM)
	}
	if err != nil {
		return false, fmt.Errorf("ending beekeeper's finished desktop CLI %d (%s) to stay under the desktop's cap of %d: %w", s.PID, s.HostID, limit, err)
	}
	by, cerr := a.caller()
	if cerr != nil {
		by = state.Party{Name: "beekeeper, outside a session"}
	}
	idle := clock(a.now, s.LastActive)
	_ = a.store.Log(event(by, "desktop.room", "ended the idle desktop CLI %d of %s (%s, idle since %s) before a spawn: the desktop runs its cap of %d CLIs",
		s.PID, s.HostID, s.Name, idle, limit))
	if _, err := fmt.Fprintf(a.out, "room: ended the idle desktop CLI %d of %s (%s, idle since %s): the desktop runs its cap of %d CLIs\n",
		s.PID, s.HostID, s.Name, idle, limit); err != nil {
		return false, err
	}
	return true, awaitExit(ctx, s.PID, roomWait)
}

// roomFor is the desktop CLI makeRoom ends: one of beekeeper's own (roomRank),
// never one of keep, the lowest rank first, then the one idle longest; nil
// when none is.
func roomFor(st *state.State, sessions []*claude.Session, t *proc.Table, now time.Time, keep []string) *claude.Session {
	var best *claude.Session
	bestRank := 0
	for _, s := range sessions {
		r, ok := roomRank(st, t, s, now)
		if !ok || slices.Contains(keep, s.HostID) {
			continue
		}
		if best == nil || r < bestRank || r == bestRank && s.LastActive.Before(best.LastActive) {
			best, bestRank = s, r
		}
	}
	return best
}

// roomRank ranks the desktop CLI of s for makeRoom: 0 a finished worker (off
// the roster, or on it without a task), 1 a parked one. Its session stays,
// and the desktop's send starts a CLI of it again. False: never ended, since
// it is no desktop CLI, its session is not one beekeeper started (the
// person's own), it holds a role, it is busy on its task, a merge of it waits
// or runs in the gate (whose outcome wakes it), or it runs a turn or a
// command or was active within stewardQuiet.
func roomRank(st *state.State, t *proc.Table, s *claude.Session, now time.Time) (int, bool) {
	p := t.ByPID[s.PID]
	switch {
	case s.HostID == "" || s.Archived || p == nil || !slices.Contains(p.Args, permissionPromptTool):
		return 0, false
	case !slices.ContainsFunc(st.Starts, func(x state.Start) bool { return x.Session == s.ID }):
		return 0, false
	case keepsRole(st, s.Party()):
		return 0, false
	case slices.ContainsFunc(st.Merges, func(m state.Merge) bool { return m.Finished.IsZero() && m.By.Is(s.Party()) }):
		return 0, false
	case len(s.Commands) > 0 || headlessTurn(t, s.ID) != "" || now.Sub(s.LastActive) < stewardQuiet:
		return 0, false
	}
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(s.Party()) })
	switch {
	case i < 0 || st.Agents[i].Task == "":
		return 0, true
	case st.Agents[i].Park != nil:
		return 1, true
	}
	return 0, false
}

// awaitExit waits up to wait for process pid to exit.
func awaitExit(ctx context.Context, pid int, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if !proc.Alive(pid) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the desktop CLI %d did not exit within %s", pid, wait)
		case <-tick.C:
		}
	}
}
