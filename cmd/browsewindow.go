package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/hypr"
)

// hyprRunning reports whether the session is a Hyprland one and chromeCtl
// runs its hyprctl; tests replace both.
var (
	hyprRunning          = hypr.Running
	chromeCtl   hypr.Ctl = hypr.Hyprctl
)

// windowWait bounds each placement of the shared Chrome window: a few
// hyprctl calls.
const windowWait = 10 * time.Second

// placement is what browse did with the shared Chrome window for its turn:
// Locked names the screen locker that ran, under which the compositor
// draws no window; Where is where the window was when it was not shown and
// To the workspace the turn showed it on; Failed says why it could not
// show it; Restored whether it went back after.
type placement struct {
	Locked   string
	Where    string
	To       string
	Failed   string
	Restored bool
	shown    *hypr.Shown
}

// showChrome brings the shared Chrome window on screen for the turn when
// the compositor does not show it (hypr.Show). Under a locked screen it
// moves nothing: no window is drawn there, wherever it sits.
func showChrome(ctx context.Context) *placement {
	p := &placement{}
	if t, err := plat.Machine.Processes(); err == nil {
		if p.Locked = claude.ScreenLocker(t); p.Locked != "" {
			return p
		}
	}
	if !hyprRunning() {
		return p
	}
	ctx, cancel := context.WithTimeout(ctx, windowWait)
	defer cancel()
	s, err := hypr.Show(ctx, chromeCtl)
	if err != nil {
		p.Failed = err.Error()
		return p
	}
	if s != nil {
		p.shown, p.Where, p.To = s, s.Where, s.To.Name
	}
	return p
}

// restore puts the window back where the turn found it, once; a stopped
// browse still restores it.
func (p *placement) restore(ctx context.Context) error {
	if p.shown == nil {
		return nil
	}
	s := p.shown
	p.shown = nil
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), windowWait)
	defer cancel()
	if err := s.Restore(ctx, chromeCtl); err != nil {
		return fmt.Errorf("putting the shared Chrome window back on workspace %s: %w", s.Before.Workspace.Name, err)
	}
	p.Restored = true
	return nil
}

// line says what browse did with the window, "" when it found it shown.
func (p placement) line() string {
	switch {
	case p.Locked != "":
		return fmt.Sprintf("screen locked: %s runs, and the compositor draws no window under the lock: the shared Chrome window was left where it is", p.Locked)
	case p.Failed != "":
		return "window not shown: " + p.Failed
	case p.To == "":
		return ""
	}
	back := "and put it back after"
	if !p.Restored {
		back = "and did not put it back"
	}
	return fmt.Sprintf("window shown: the shared Chrome window was on %s; the turn showed it floating on workspace %s %s", p.Where, p.To, back)
}
