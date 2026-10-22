// Package hypr gives the shared Chrome window a drawn surface for a browse
// turn under Hyprland and puts it back after: Chrome draws nothing in a
// window the compositor does not show (on a workspace not shown, under a
// special workspace shown over its own, behind the front window of a
// monocle workspace), and a turn's draw checks, screenshots and clicks fail
// there. The surface is a headless output of its own, placed where no edge
// touches the person's monitors: the window is drawn there and never
// appears on the person's screen, and their monitors, workspaces and focus
// stay as they are. It never changes Hyprland's configuration files: the
// output's monitor and workspace rules are set at runtime only.
package hypr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
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

// Output is the headless output a turn draws the window on, and Surface
// its workspace: named, so the person's workspace numbers stay theirs.
const (
	Output  = "BEEKEEPER-BROWSE"
	Surface = "beekeeper-browse"
)

// Size is the output's mode: big enough for a page's screenshot.
var Size = [2]int{1280, 800}

// position is the output's place in the layout: far from any monitor, so
// no edge of the person's monitors leads the cursor onto it.
const position = "-30000x-30000"

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

// surface is the output's workspace.
var surface = Workspace{Name: Surface}

// Window is a client as hyprctl clients names it.
type Window struct {
	Address        string    `json:"address"`
	Class          string    `json:"class"`
	Workspace      Workspace `json:"workspace"`
	Floating       bool      `json:"floating"`
	Fullscreen     int       `json:"fullscreen"`
	Monitor        int       `json:"monitor"`
	FocusHistoryID int       `json:"focusHistoryID"`
}

// Monitor is a monitor as hyprctl monitors names it: X and Y in layout
// coordinates, Width and Height in pixels.
type Monitor struct {
	ID               int       `json:"id"`
	Name             string    `json:"name"`
	X                int       `json:"x"`
	Y                int       `json:"y"`
	Width            int       `json:"width"`
	Height           int       `json:"height"`
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

// output is the headless output, nil when there is none.
func (l layout) output() *Monitor {
	for i, m := range l.monitors {
		if m.Name == Output {
			return &l.monitors[i]
		}
	}
	return nil
}

// view is what the person sees: their monitors' place, size, shown
// workspaces and focus, the headless output left out.
func (l layout) view() []Monitor {
	var v []Monitor
	for _, m := range l.monitors {
		if m.Name != Output {
			v = append(v, m)
		}
	}
	return v
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

// Shown is a window a placement moved onto the headless output, with what
// it was before. It is kept as JSON while the turn runs, so a browse that
// died mid-turn still puts the window back.
type Shown struct {
	// Before is the window as it was; Where says where that was.
	Before Window `json:"before"`
	Where  string `json:"where"`
	// Created says the placement created the output, which Restore removes.
	Created bool `json:"created"`
	// Left says why Restore left the window where it was: closed, or moved
	// by the person to another workspace; "" when it put it back.
	Left string `json:"left,omitempty"`
}

// Show moves Chrome's browser window onto the headless output when the
// compositor does not show it, creating the output when there is none:
// drawn there, never on the person's screen, the focus left where it is.
// An output that changed the person's view (their monitors' place, shown
// workspaces or focus) or took a numbered workspace is removed again and
// the window left where it is. It returns nil when there is no browser
// window or it is shown already; a step that fails is undone and its error
// names where the window is.
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
	s := &Shown{Before: *w, Where: where}
	fail := func(err error) (*Shown, error) {
		return nil, fmt.Errorf("the shared Chrome window is on %s; drawing it on the off-screen output %s failed: %w",
			where, Output, joinErr(err, s.removeOutput(ctx, ctl)))
	}
	if l.output() == nil {
		if err := rules.run(ctx, ctl); err != nil {
			return fail(err)
		}
		s.Created = true
		if err := run(ctx, ctl, "output", "create", "headless", Output); err != nil {
			return fail(err)
		}
		after, err := read(ctx, ctl)
		if err != nil {
			return fail(err)
		}
		if err := checkOutput(l, after); err != nil {
			return fail(err)
		}
	}
	if err := moveTo(w.Address, surface).run(ctx, ctl); err != nil {
		return fail(err)
	}
	return s, nil
}

// checkOutput refuses an output that changed the person's view from
// before, or whose workspace is not the named Surface.
func checkOutput(before, after layout) error {
	o := after.output()
	switch {
	case o == nil:
		return fmt.Errorf("hyprctl output create headless %s: no such output after", Output)
	case o.ActiveWorkspace.Name != Surface:
		return fmt.Errorf("the output %s shows workspace %s, not %s: its workspace rule did not apply", Output, o.ActiveWorkspace.Name, Surface)
	case !slices.Equal(before.view(), after.view()):
		return fmt.Errorf("creating the output %s changed the person's monitors to %+v from %+v", Output, after.view(), before.view())
	}
	return nil
}

// Restore puts the window back on its own workspace, and any other window
// on the output's workspace with it (one the turn opened); then it removes
// the output, unless a window failed to move off it, which
// removing the output would put on the person's screen. One closed since,
// or moved by the person to a third workspace, is left where it is, and
// Left says so.
func (s *Shown) Restore(ctx context.Context, ctl Ctl) error {
	if s == nil {
		return nil
	}
	l, err := read(ctx, ctl)
	if err != nil {
		return err
	}
	var errs []error
	s.Left = "closed"
	for _, w := range l.windows {
		if w.Workspace.Name == Surface {
			errs = append(errs, moveTo(w.Address, s.Before.Workspace).run(ctx, ctl))
		}
		switch {
		case w.Address != s.Before.Address:
		case w.Workspace.Name == Surface, w.Workspace.ID == s.Before.Workspace.ID:
			s.Left = ""
		default:
			s.Left = "moved by the person to workspace " + w.Workspace.Name
		}
	}
	if err := joinErr(errs...); err != nil {
		return fmt.Errorf("%w; the output %s stays until its windows are moved off it", err, Output)
	}
	// The output is beekeeper's by its name: one a died turn left is
	// removed too.
	s.Created = l.output() != nil
	return s.removeOutput(ctx, ctl)
}

// removeOutput removes the output the placement created, once.
func (s *Shown) removeOutput(ctx context.Context, ctl Ctl) error {
	if !s.Created {
		return nil
	}
	s.Created = false
	return run(ctx, ctl, "output", "remove", Output)
}

// rules give the output its far-off place and Size, and its workspace the
// name Surface, before it exists: in a Lua configuration once per
// configuration load, in a classic one as keywords.
var rules = setting{
	lua: fmt.Sprintf(`if not beekeeper_browse then beekeeper_browse = true; `+
		`hl.monitor({ output = %q, mode = "%dx%d", position = %q, scale = "1" }); `+
		`hl.workspace_rule({ workspace = %q, monitor = %q, default = true }) end`,
		Output, Size[0], Size[1], position, "name:"+Surface, Output),
	classic: [][2]string{
		{"monitor", fmt.Sprintf("%s,%dx%d,%s,1", Output, Size[0], Size[1], position)},
		{"workspace", fmt.Sprintf("name:%s,monitor:%s,default:true", Surface, Output)},
	},
}

// setting is runtime configuration in both of Hyprland's syntaxes: a Lua
// chunk hyprctl eval takes, and the keywords hyprctl keyword takes
// otherwise.
type setting struct {
	lua     string
	classic [][2]string
}

func (s setting) run(ctx context.Context, ctl Ctl) error {
	lerr := run(ctx, ctl, "eval", s.lua)
	if lerr == nil {
		return nil
	}
	for _, k := range s.classic {
		if err := run(ctx, ctl, "keyword", k[0], k[1]); err != nil {
			return joinErr(lerr, err)
		}
	}
	return nil
}

// run runs a hyprctl command that answers ok.
func run(ctx context.Context, ctl Ctl, args ...string) error {
	out, err := ctl(ctx, args...)
	if err == nil && strings.TrimSpace(string(out)) == "ok" {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if err != nil {
		msg = err.Error()
	}
	return fmt.Errorf("hyprctl %s %s: %s", args[0], args[1], firstLine(msg))
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
