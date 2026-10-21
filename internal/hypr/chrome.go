// Package hypr shows the shared Chrome window on screen for a browse turn
// under Hyprland and puts it back after: Chrome draws nothing in a window
// the compositor does not show (on a workspace not shown, under a special
// workspace shown over its own, behind the front window of a monocle
// workspace), and a turn's draw checks, screenshots and clicks fail there.
// It only dispatches window moves; it never reads or changes Hyprland's
// configuration (keybinds, rules, workspace settings).
package hypr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Ctl runs hyprctl with args and returns what it prints; tests replace it.
type Ctl func(ctx context.Context, args ...string) ([]byte, error)

// Hyprctl is the Ctl of the running compositor.
func Hyprctl(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "hyprctl", args...).Output() //nolint:gosec // hyprctl on PATH, arguments beekeeper builds
}

// Running reports whether the session is a Hyprland one.
func Running() bool { return os.Getenv("HYPRLAND_INSTANCE_SIGNATURE") != "" }

// browserClasses are the Wayland classes of Chrome's browser windows; an
// installed web app's window (chrome-<app>-Default) is not one.
var browserClasses = []string{"google-chrome", "google-chrome-beta", "google-chrome-unstable", "chromium", "chromium-browser"}

// Size is the size the shown window gets, at most: big enough for a page's
// screenshot, small enough to leave most of the person's screen in view.
var Size = [2]int{1280, 800}

// margin keeps the shown window off the screen's edge.
const margin = 8

// Workspace is a workspace as hyprctl names it.
type Workspace struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// selector is the workspace's dispatcher selector, which recreates it when
// it went with its last window: a special one by its name, a numbered one
// by its number, a named one as name:<name>.
func (w Workspace) selector() string {
	if strings.HasPrefix(w.Name, "special:") || w.Name == strconv.Itoa(w.ID) {
		return w.Name
	}
	return "name:" + w.Name
}

// Window is a client as hyprctl clients names it.
type Window struct {
	Address        string    `json:"address"`
	Class          string    `json:"class"`
	Workspace      Workspace `json:"workspace"`
	At             [2]int    `json:"at"`
	Size           [2]int    `json:"size"`
	Floating       bool      `json:"floating"`
	Fullscreen     int       `json:"fullscreen"`
	Monitor        int       `json:"monitor"`
	FocusHistoryID int       `json:"focusHistoryID"`
}

// Monitor is a monitor as hyprctl monitors names it: X and Y in layout
// coordinates, Width and Height in pixels, Reserved the bars' left, top,
// right and bottom.
type Monitor struct {
	ID               int       `json:"id"`
	Name             string    `json:"name"`
	X                int       `json:"x"`
	Y                int       `json:"y"`
	Width            int       `json:"width"`
	Height           int       `json:"height"`
	Scale            float64   `json:"scale"`
	Reserved         [4]int    `json:"reserved"`
	ActiveWorkspace  Workspace `json:"activeWorkspace"`
	SpecialWorkspace Workspace `json:"specialWorkspace"`
	Focused          bool      `json:"focused"`
}

// shows is the workspace the monitor shows: its special workspace when one
// is shown over the active one, which it hides.
func (m Monitor) shows() Workspace {
	if m.SpecialWorkspace.ID != 0 {
		return m.SpecialWorkspace
	}
	return m.ActiveWorkspace
}

// layout is the compositor's state a placement reads.
type layout struct {
	windows  []Window
	monitors []Monitor
	monocle  bool
}

func read(ctx context.Context, ctl Ctl) (layout, error) {
	var l layout
	if err := query(ctx, ctl, &l.windows, "clients"); err != nil {
		return l, err
	}
	if err := query(ctx, ctl, &l.monitors, "monitors"); err != nil {
		return l, err
	}
	var opt struct {
		Str string `json:"str"`
	}
	if err := query(ctx, ctl, &opt, "getoption", "general:layout"); err != nil {
		return l, err
	}
	l.monocle = opt.Str == "monocle"
	return l, nil
}

func query(ctx context.Context, ctl Ctl, v any, args ...string) error {
	out, err := ctl(ctx, append([]string{"-j"}, args...)...)
	if err != nil {
		return fmt.Errorf("hyprctl %s: %w", args[0], err)
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("hyprctl %s: %w", args[0], err)
	}
	return nil
}

// browser is Chrome's most recently focused browser window, the one the
// extension opens a turn's tabs in; nil without one.
func (l layout) browser() *Window {
	var b *Window
	for i, w := range l.windows {
		for _, c := range browserClasses {
			if strings.EqualFold(w.Class, c) && (b == nil || w.FocusHistoryID < b.FocusHistoryID) {
				b = &l.windows[i]
			}
		}
	}
	return b
}

// monitor is the window's monitor, the focused one when it names none.
func (l layout) monitor(w Window) (Monitor, bool) {
	var focused *Monitor
	for i, m := range l.monitors {
		if m.ID == w.Monitor {
			return m, true
		}
		if m.Focused {
			focused = &l.monitors[i]
		}
	}
	if focused != nil {
		return *focused, true
	}
	return Monitor{}, false
}

// where says where the window is when the compositor does not show it, ""
// when it does: on a shown workspace, floating, fullscreen or, on a monocle
// workspace, its front window, and no other window fullscreen there.
func (l layout) where(w Window) string {
	ws := fmt.Sprintf("workspace %s", w.Workspace.Name)
	var on *Monitor
	for i, m := range l.monitors {
		if m.shows().ID == w.Workspace.ID {
			on = &l.monitors[i]
		}
		if m.ActiveWorkspace.ID == w.Workspace.ID && m.SpecialWorkspace.ID != 0 {
			return fmt.Sprintf("%s, under the special workspace %s shown over it on %s", ws, m.SpecialWorkspace.Name, m.Name)
		}
	}
	if on == nil {
		return ws + ", not shown on any monitor"
	}
	for _, o := range l.windows {
		if o.Address == w.Address || o.Workspace.ID != w.Workspace.ID {
			continue
		}
		if o.Fullscreen > 0 && w.Fullscreen == 0 {
			return fmt.Sprintf("%s, behind the fullscreen window %s", ws, o.Class)
		}
		if l.monocle && !w.Floating && w.Fullscreen == 0 && !o.Floating && o.FocusHistoryID < w.FocusHistoryID {
			return fmt.Sprintf("%s, behind its front window %s (monocle layout)", ws, o.Class)
		}
	}
	return ""
}

// Shown is a window a placement moved on screen, with what it was before.
type Shown struct {
	// Before is the window as it was; Where says where that was.
	Before Window
	Where  string
	// To is the workspace it was shown on.
	To Workspace
	// Left says why Restore left the window where it was: closed, or moved
	// by the person to another workspace; "" when it put it back.
	Left string
	// moved and floated are the steps done, which Restore undoes.
	moved, floated, placed bool
}

// Show brings Chrome's browser window on screen when the compositor does
// not show it: onto the workspace its monitor shows, floating at Size in
// the monitor's lower right corner, the focus left where it is. It returns
// nil when there is no browser window or it is shown already; a step that
// fails is undone and its error names where the window is.
func Show(ctx context.Context, ctl Ctl) (*Shown, error) {
	l, err := read(ctx, ctl)
	if err != nil {
		return nil, err
	}
	w := l.browser()
	if w == nil {
		return nil, nil
	}
	where := l.where(*w)
	if where == "" {
		return nil, nil
	}
	m, ok := l.monitor(*w)
	if !ok {
		return nil, fmt.Errorf("the shared Chrome window is on %s, and no monitor to show it on", where)
	}
	s := &Shown{Before: *w, Where: where, To: m.shows()}
	at, size := corner(m)
	addr := w.Address
	steps := []struct {
		needed bool
		done   *bool
		ds     []dispatch
	}{
		{s.To.ID != w.Workspace.ID, &s.moved, []dispatch{moveTo(addr, s.To)}},
		{!w.Floating, &s.floated, []dispatch{float(addr, true)}},
		{true, &s.placed, []dispatch{resize(addr, size), movePixel(addr, at)}},
	}
	for _, st := range steps {
		if !st.needed {
			continue
		}
		// A step is marked done before its dispatches: one that half ran
		// is undone too.
		*st.done = true
		for _, d := range st.ds {
			if err := d.run(ctx, ctl); err != nil {
				rerr := s.undo(ctx, ctl)
				return nil, fmt.Errorf("the shared Chrome window is on %s; showing it failed: %w", where, joinErr(err, rerr))
			}
		}
	}
	return s, nil
}

// Restore puts the window back as it was: its size and position when it
// floated, tiled again when it did not, on its own workspace. One the
// person moved back to its own workspace is only given its float and size
// back; one closed since, or moved to a third workspace, is left where it
// is, and Left says so.
func (s *Shown) Restore(ctx context.Context, ctl Ctl) error {
	if s == nil {
		return nil
	}
	l, err := read(ctx, ctl)
	if err != nil {
		return err
	}
	for _, w := range l.windows {
		if w.Address != s.Before.Address {
			continue
		}
		switch w.Workspace.ID {
		case s.To.ID:
		case s.Before.Workspace.ID:
			s.moved = false
		default:
			s.Left = "moved by the person to workspace " + w.Workspace.Name
			return nil
		}
		return s.undo(ctx, ctl)
	}
	s.Left = "closed"
	return nil
}

func (s *Shown) undo(ctx context.Context, ctl Ctl) error {
	addr := s.Before.Address
	var errs []error
	if s.placed && s.Before.Floating {
		errs = append(errs, resize(addr, s.Before.Size).run(ctx, ctl), movePixel(addr, s.Before.At).run(ctx, ctl))
	}
	if s.floated {
		errs = append(errs, float(addr, false).run(ctx, ctl))
	}
	if s.moved {
		errs = append(errs, moveTo(addr, s.Before.Workspace).run(ctx, ctl))
	}
	s.moved, s.floated, s.placed = false, false, false
	return joinErr(errs...)
}

// corner is the window's position and size in the monitor's lower right
// corner, inside its bars.
func corner(m Monitor) (at, size [2]int) {
	scale := m.Scale
	if scale <= 0 {
		scale = 1
	}
	right := m.X + int(float64(m.Width)/scale) - m.Reserved[2] - margin
	bottom := m.Y + int(float64(m.Height)/scale) - m.Reserved[3] - margin
	left, top := m.X+m.Reserved[0]+margin, m.Y+m.Reserved[1]+margin
	size = [2]int{min(Size[0], right-left), min(Size[1], bottom-top)}
	return [2]int{right - size[0], bottom - size[1]}, size
}

// dispatch is one dispatcher call in both of Hyprland's syntaxes: the Lua
// one of a Lua configuration, and the classic one hyprctl takes otherwise.
type dispatch struct{ lua, classic string }

// run tries the Lua call, then the classic one; hyprctl answers ok to the
// one its configuration takes.
func (d dispatch) run(ctx context.Context, ctl Ctl) error {
	var last string
	for _, call := range []string{d.lua, d.classic} {
		out, err := ctl(ctx, "dispatch", call)
		if err == nil && strings.TrimSpace(string(out)) == "ok" {
			return nil
		}
		last = strings.TrimSpace(string(out))
		if err != nil {
			last = err.Error()
		}
	}
	return fmt.Errorf("hyprctl dispatch %s: %s", d.classic, firstLine(last))
}

func window(addr string) string { return fmt.Sprintf("window = %q", "address:"+addr) }

func moveTo(addr string, ws Workspace) dispatch {
	return dispatch{
		fmt.Sprintf("hl.dsp.window.move({ workspace = %q, follow = false, %s })", ws.selector(), window(addr)),
		fmt.Sprintf("movetoworkspacesilent %s,address:%s", ws.selector(), addr),
	}
}

func float(addr string, on bool) dispatch {
	action, classic := "set", "setfloating"
	if !on {
		action, classic = "unset", "settiled"
	}
	return dispatch{
		fmt.Sprintf("hl.dsp.window.float({ action = %q, %s })", action, window(addr)),
		fmt.Sprintf("%s address:%s", classic, addr),
	}
}

func resize(addr string, size [2]int) dispatch {
	return dispatch{
		fmt.Sprintf("hl.dsp.window.resize({ x = %d, y = %d, %s })", size[0], size[1], window(addr)),
		fmt.Sprintf("resizewindowpixel exact %d %d,address:%s", size[0], size[1], addr),
	}
}

func movePixel(addr string, at [2]int) dispatch {
	return dispatch{
		fmt.Sprintf("hl.dsp.window.move({ x = %d, y = %d, %s })", at[0], at[1], window(addr)),
		fmt.Sprintf("movewindowpixel exact %d %d,address:%s", at[0], at[1], addr),
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func joinErr(errs ...error) error {
	var msgs []string
	for _, e := range errs {
		if e != nil {
			msgs = append(msgs, e.Error())
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(msgs, "; "))
}
