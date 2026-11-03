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
// it was (hypr.Shown.Left); Opened counts the windows Chrome opened during
// the turn that were still open after it (hypr.Shown.Opened), which went
// to Back, the shared window's workspace; Recovered is
// the workspace a window a died turn left on the output went back to.
// Yielded says how the person turned to the window during the turn, which
// gave it back to them; Watch names what failed while following them.
type placement struct {
	Locked    string
	Yielded   string
	Watch     string
	Where     string
	To        string
	Failed    string
	Restored  bool
	Left      string
	Opened    int
	Back      string
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

// errPersonAtChrome opens the refusal of a browse while the person works at
// Chrome, and the error of a turn that gave the shared window back to them.
var errPersonAtChrome = errors.New("person at Chrome")

// awayProbe is the command a caller parks on until the person leaves
// Chrome: browse away.
const awayProbe = "beekeeper browse away"

// awaySample is how long browse watches the person's input before a turn
// on a Chrome window in their view; presencePoll paces the watch over the
// turn. Tests shorten both.
var (
	awaySample   = 5 * time.Second
	presencePoll = 2 * time.Second
)

// presence is the person's input as browse watches it: input counts while
// it arrived within quiet. An input browse cannot read counts as arriving.
// The zero presence watches nothing and never counts input.
type presence struct {
	watched bool
	start   time.Time
	last    func() time.Time
	quiet   time.Duration
}

// watchPresence watches the person's keyboards and pointers until ctx ends,
// quiet being desktop.typingQuiet; a negative quiet watches nothing.
func watchPresence(ctx context.Context, quiet time.Duration) presence {
	if quiet < 0 {
		return presence{}
	}
	p := presence{watched: true, quiet: quiet}
	if last, err := desktopInput(ctx); err == nil {
		// The watch's start is its first answer: only input after it counts.
		p.last, p.start = last, last()
	}
	return p
}

// typing reports whether the person typed or pointed within quiet.
func (p presence) typing(now time.Time) bool {
	switch {
	case !p.watched:
		return false
	case p.last == nil:
		return true
	}
	l := p.last()
	return l.After(p.start) && now.Sub(l) < p.quiet
}

// how says what the watch saw of the person's input.
func (p presence) how() string {
	if p.last == nil {
		return "their keyboards and pointers cannot be read, which counts as their input"
	}
	return "their keyboard or pointer was in use"
}

// personAtChromeErr refuses a browse, or ends its turn, while the person
// works at Chrome (why), naming the probe a caller parks on.
func personAtChromeErr(why string) error {
	return fmt.Errorf("%w: %s: browse leaves the shared Chrome window to the person. "+
		"Park until they leave Chrome and run the steps again then, with a timer on the probe %q "+
		"(beekeeper timer add 1m \"person away from Chrome: run the browse steps again\" --probe %q --wake \"<agent>\" --until 12h), not a retry loop",
		errPersonAtChrome, why, awayProbe, awayProbe)
}

// atChrome watches the person's input from now until ctx ends and refuses a
// turn while they work at Chrome: a Chrome window in their view (focused,
// focused just before, or on a workspace a monitor shows) and their input within awaySample. With no
// Chrome window in view it starts the turn without waiting. Not under
// Hyprland, it watches nothing and refuses nothing.
func atChrome(ctx context.Context, quiet time.Duration) (presence, error) {
	if !hyprRunning() {
		return presence{}, nil
	}
	p := watchPresence(ctx, quiet)
	if !p.watched {
		return p, nil
	}
	vctx, cancel := context.WithTimeout(ctx, windowWait)
	why, err := hypr.InView(vctx, chromeCtl)
	cancel()
	if err != nil {
		// A compositor that cannot be asked counts as Chrome in view.
		why = fmt.Sprintf("the compositor could not be asked where Chrome is (%v)", err)
	}
	if why == "" {
		return p, nil
	}
	if p.last != nil {
		t := time.NewTimer(awaySample)
		select {
		case <-ctx.Done():
			t.Stop()
			return p, ctx.Err()
		case <-t.C:
		}
	}
	if !p.typing(time.Now()) {
		return p, nil
	}
	return p, personAtChromeErr(fmt.Sprintf("%s and %s", why, p.how()))
}

// watch follows the person over the turn until the returned stop: while
// they type or point, the windows Chrome opens stay on their screen, and
// once they turn to the shared window's workspace the turn yields, cancel
// stops it with errPersonAtChrome and the window goes back at once. stop
// waits for the watch to end, so the placement is the caller's again.
func (p *placement) watch(ctx context.Context, pres presence, hold time.Duration, cancel context.CancelCauseFunc) (stop func()) {
	if p.shown == nil || !pres.watched {
		return func() {}
	}
	ctx, end := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(presencePoll)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			if p.follow(ctx, pres, hold, cancel) {
				return
			}
		}
	}()
	return func() { end(); <-done }
}

// follow is one look of watch: it sets the redirect by the person's input
// and yields the window when they turned to it, reporting whether it did.
func (p *placement) follow(ctx context.Context, pres presence, hold time.Duration, cancel context.CancelCauseFunc) bool {
	ctx, done := context.WithTimeout(ctx, windowWait)
	defer done()
	typing := pres.typing(time.Now())
	if err := p.shown.Redirect(ctx, chromeCtl, !typing, hold); err != nil && p.Watch == "" {
		p.Watch = err.Error()
	}
	if !typing {
		return false
	}
	turned, err := p.shown.Turned(ctx, chromeCtl)
	if err != nil || turned == "" {
		return false
	}
	p.Yielded = turned
	cancel(personAtChromeErr(fmt.Sprintf("during the turn %s and %s, so the turn stopped and the window went back to them", turned, pres.how())))
	if err := p.restore(ctx); err != nil && p.Watch == "" {
		p.Watch = err.Error()
	}
	return true
}

// showChrome readies the off-screen output for the turn (hypr.Show): every
// window Chrome opens while redirected, until restore or hold at the
// latest, is drawn there, and the shared window too when the compositor
// does not show it; it keeps the placement in record until restore. A
// record a died turn left is put back first. Under a locked screen it moves
// nothing: no window is drawn there, wherever it sits.
func showChrome(ctx context.Context, record string, hold time.Duration) *placement {
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
	s, err := hypr.Show(ctx, chromeCtl, hold)
	if err != nil {
		p.Failed = err.Error()
		return p
	}
	if s == nil {
		return p
	}
	p.shown = s
	if s.Moved {
		p.Where, p.To = s.Where, hypr.Output
	}
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
	p.Restored, p.Left, p.Opened, p.Back = s.Left == "", s.Left, s.Opened, s.Before.Workspace.Name
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
	if p.Yielded != "" {
		lines = append(lines, fmt.Sprintf("window yielded: during the turn %s while the person worked at the desktop: the turn stopped and the window went back to them", p.Yielded))
	}
	if p.Watch != "" {
		lines = append(lines, "person not followed: "+p.Watch)
	}
	if p.Opened > 0 {
		lines = append(lines, fmt.Sprintf("windows opened: %d window(s) Chrome opened during the turn were drawn on the off-screen output %s, "+
			"never on the person's screen, and were still open after it: they went to the shared window's workspace %s", p.Opened, hypr.Output, p.Back))
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
