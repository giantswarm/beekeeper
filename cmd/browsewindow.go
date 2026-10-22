package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
// To the off-screen output the turn drew it on; Failed says why it could
// not; Restored whether it went back after, and Left why it was left where
// it was (hypr.Shown.Left); Recovered is the workspace a window a died
// turn left on the output went back to.
type placement struct {
	Locked    string
	Where     string
	To        string
	Failed    string
	Restored  bool
	Left      string
	Recovered string
	shown     *hypr.Shown
	record    string
}

// screenLocker names the screen locker that runs, "" when none does or the
// process table cannot be read: under it the compositor draws no window
// but the lock's. Tests replace it.
var screenLocker = machineScreenLocker

// machineScreenLocker reads the screen locker from the machine's process
// table.
func machineScreenLocker() string {
	t, err := plat.Machine.Processes()
	if err != nil {
		return ""
	}
	return claude.ScreenLocker(t)
}

// errScreenLocked opens the refusal of a browse under a running screen
// locker.
var errScreenLocked = errors.New("screen locked")

// screenLockedErr refuses a browse before its turn starts while a screen
// locker runs: the compositor draws no window under the lock, so no draw
// check, screenshot or click of the turn could pass. It names the probe a
// caller parks on until the screen unlocks; the lock stays the person's.
// nil when no locker runs or the process table cannot be read.
func screenLockedErr() error {
	l := screenLocker()
	if l == "" {
		return nil
	}
	return fmt.Errorf("%w: %s runs, and the compositor draws no window under the lock: browse started no turn. "+
		"Park until the screen unlocks and run the steps again then, with a timer on the probe %q "+
		"(beekeeper timer add 1m \"screen unlocked: run the browse steps again\" --probe %q --wake \"<agent>\" --until 12h), not a retry loop; "+
		"browse never touches the lock", errScreenLocked, l, unlockedProbe(l), unlockedProbe(l))
}

// unlockedProbe is a shell command that exits 0 once the locker no longer
// runs.
func unlockedProbe(locker string) string { return "! pgrep -x " + locker }

// showChrome gives the shared Chrome window a drawn surface for the turn
// when the compositor does not show it (hypr.Show), off the person's
// screen, and keeps what it moved in record until restore. A record a died
// turn left is put back first. Under a locked screen it moves nothing: no
// window is drawn there, wherever it sits.
func showChrome(ctx context.Context, record string) *placement {
	p := &placement{record: record}
	if p.Locked = screenLocker(); p.Locked != "" {
		return p
	}
	if !hyprRunning() {
		return p
	}
	ctx, cancel := context.WithTimeout(ctx, windowWait)
	defer cancel()
	if left, err := readShown(record); err != nil || left != nil {
		if err == nil {
			err = left.Restore(ctx, chromeCtl)
		}
		if err != nil {
			p.Failed = fmt.Sprintf("putting back the window a died turn left on the off-screen output %s (%s): %v", hypr.Output, record, err)
			return p
		}
		if left.Left == "" {
			p.Recovered = left.Before.Workspace.Name
		}
		_ = os.Remove(record)
	}
	s, err := hypr.Show(ctx, chromeCtl)
	if err != nil {
		p.Failed = err.Error()
		return p
	}
	if s == nil {
		return p
	}
	p.shown, p.Where, p.To = s, s.Where, hypr.Output
	if err := writeShown(record, s); err != nil {
		p.Failed = err.Error()
		_ = p.restore(ctx)
	}
	return p
}

// readShown reads the window a turn moved, nil without a record.
func readShown(record string) (*hypr.Shown, error) {
	b, err := os.ReadFile(record) //nolint:gosec // beekeeper's own state file
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s hypr.Shown
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", record, err)
	}
	return &s, nil
}

// writeShown records the window a turn moved until it is put back.
func writeShown(record string, s *hypr.Shown) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(record), 0o700); err != nil {
		return fmt.Errorf("recording the shared Chrome window's place: %w", err)
	}
	if err := os.WriteFile(record, b, 0o600); err != nil {
		return fmt.Errorf("recording the shared Chrome window's place: %w", err)
	}
	return nil
}

// restore puts the window back where the turn found it, once, and drops
// the record; a stopped browse still restores it. A window not put back
// keeps its record for the next browse.
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
	p.Restored, p.Left = s.Left == "", s.Left
	if err := os.Remove(p.record); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// line says what browse did with the window, "" when it found it shown.
func (p placement) line() string {
	var lines []string
	if p.Recovered != "" {
		lines = append(lines, fmt.Sprintf("window put back: a died turn had left the shared Chrome window on the off-screen output %s; it went back to workspace %s", hypr.Output, p.Recovered))
	}
	switch {
	case p.Locked != "":
		lines = append(lines, fmt.Sprintf("screen locked: %s runs, and the compositor draws no window under the lock: the shared Chrome window was left where it is", p.Locked))
	case p.Failed != "":
		lines = append(lines, "window not shown: "+p.Failed)
	case p.To != "":
		lines = append(lines, fmt.Sprintf("window shown: the shared Chrome window was on %s; the turn %s", p.Where, p.tried()))
	}
	return strings.Join(lines, "\n")
}

// tried says where the turn drew the window and how it put it back.
func (p placement) tried() string {
	back := "and put it back after"
	switch {
	case p.Left != "":
		back = "and left it where it was after: " + p.Left + " during the turn"
	case !p.Restored:
		back = "and did not put it back"
	}
	return fmt.Sprintf("drew it on the off-screen output %s, never on the person's screen, %s", p.To, back)
}
