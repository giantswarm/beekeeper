package cmd

import (
	"context"
	"fmt"
	"os"
	"slices"
	"syscall"
	"time"

	"github.com/spf13/cobra"

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
// beekeeper ends one of its own finished CLIs first: a desktop CLI of a
// session it started, idle, with no task and no role (stewards' rule), idle
// longest, never one of keep (local_ ids). With none to end it starts
// nothing: the error names the cap.
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
		return false, fmt.Errorf("the desktop runs its cap of %d CLIs and beekeeper runs none of its own finished to end: it starts none, "+
			"since at the cap the desktop pauses the CLI idle longest, the person's own sessions included", limit)
	}
	pr, err := os.FindProcess(s.PID)
	if err == nil {
		err = pr.Signal(syscall.SIGTERM)
	}
	if err != nil {
		return false, fmt.Errorf("ending beekeeper's finished desktop CLI %d (%s) to stay under the desktop's cap of %d: %w", s.PID, s.HostID, limit, err)
	}
	_ = a.store.Log(event(watchParty, "desktop.room", "ended the finished desktop CLI %d of %s, idle since %s: the desktop runs its cap of %d CLIs",
		s.PID, s.HostID, s.LastActive.Format(time.DateTime), limit))
	if _, err := fmt.Fprintf(a.out, "room: ended beekeeper's finished desktop CLI %d of %s (idle since %s), the desktop at its cap of %d CLIs\n",
		s.PID, s.HostID, s.LastActive.Format(time.DateTime), limit); err != nil {
		return false, err
	}
	return true, awaitExit(ctx, s.PID, roomWait)
}

// agentRoomCmd frees room under the desktop's cap of CLIs by hand.
func (a *app) agentRoomCmd() *cobra.Command {
	var free int
	c := &cobra.Command{
		Use:    "room",
		Short:  "Free room under the desktop's cap of CLIs from beekeeper's own finished CLIs",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if free < 1 {
				return usageErr("--free %d: at least 1", free)
			}
			return a.makeRoomFor(cmd.Context(), free)
		},
	}
	c.Flags().IntVar(&free, "free", 1, "the CLIs the desktop is to have room for")
	return c
}

// roomFor is the desktop CLI makeRoom ends: one of beekeeper's own finished
// ones (stewards' rule: a session it started, idle, no task, no role, no
// headless turn), never one of keep, idle longest; nil when none is.
func roomFor(st *state.State, sessions []*claude.Session, t *proc.Table, now time.Time, keep []string) *claude.Session {
	var own []*claude.Session
	for _, s := range sessions {
		if !slices.Contains(keep, s.HostID) && stewards(st, t, s, "", now) {
			own = append(own, s)
		}
	}
	if len(own) == 0 {
		return nil
	}
	return slices.MinFunc(own, func(x, y *claude.Session) int { return x.LastActive.Compare(y.LastActive) })
}

// awaitExit waits up to wait for process pid to exit.
func awaitExit(ctx context.Context, pid int, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if syscall.Kill(pid, 0) != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the desktop CLI %d did not exit within %s", pid, wait)
		case <-tick.C:
		}
	}
}
