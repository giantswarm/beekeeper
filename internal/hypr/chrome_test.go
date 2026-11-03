package hypr

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// fake is a compositor: its windows and monitors answer hyprctl's queries;
// it creates and removes the headless output, moves windows, and records
// every command it takes. lua says its configuration is a Lua one, which
// refuses classic dispatches and keywords (a classic one refuses Lua
// calls and eval); refuse answers each command holding one of its words
// with an error text instead of ok; onCreate changes the compositor when
// the output is created (a numbered workspace, a focus that moved);
// redirect says the window rule of a turn is on.
type fake struct {
	windows  []Window
	monitors []Monitor
	layout   string
	classic  bool
	calls    []string
	refuse   []string
	onCreate func(f *fake, out *Monitor)
	redirect bool
}

// open maps a window Chrome opens: on the output's workspace while the
// redirect is on, else on the workspace the person's focused monitor shows.
func (f *fake) open(w Window) {
	w.Workspace = f.monitors[0].ActiveWorkspace
	if f.redirect && regexp.MustCompile(browserPattern()).MatchString(w.Class) {
		w.Workspace = surface
	}
	f.windows = append(f.windows, w)
}

var moveRE = regexp.MustCompile(`(?:workspace = "|movetoworkspacesilent )(?:name:)?([^",]+)[",].*address:(0x[0-9a-f]+)`)

func (f *fake) ctl(_ context.Context, args ...string) ([]byte, error) {
	switch {
	case slices.Equal(args, []string{"-j", "clients"}):
		return json.Marshal(f.windows)
	case slices.Equal(args, []string{"-j", "monitors"}):
		return json.Marshal(f.monitors)
	case slices.Equal(args, []string{"-j", "getoption", "general:layout"}):
		return json.Marshal(map[string]any{"option": "general:layout", "str": f.layout, "set": true})
	}
	call := strings.Join(args, " ")
	f.calls = append(f.calls, call)
	for _, r := range f.refuse {
		if strings.Contains(call, r) {
			return []byte("error: refused\nmore"), nil
		}
	}
	lua := args[0] == evalCmd || strings.HasPrefix(call, "dispatch hl.dsp.")
	if slices.Contains([]string{evalCmd, "keyword", "dispatch"}, args[0]) && lua == f.classic {
		return []byte("error: not this syntax"), nil
	}
	switch {
	case args[0] == evalCmd && strings.Contains(args[1], "set_enabled(true)"):
		f.redirect = true
	case args[0] == evalCmd && strings.HasPrefix(args[1], redirectOff):
		f.redirect = false
	case slices.Equal(args, []string{"output", "create", "headless", Output}):
		out := Monitor{ID: 1, Name: Output, X: -30000, Y: -30000, Width: Size[0], Height: Size[1], ActiveWorkspace: surface}
		if f.onCreate != nil {
			f.onCreate(f, &out)
		}
		f.monitors = append(f.monitors, out)
	case slices.Equal(args, []string{"output", "remove", Output}):
		f.monitors = slices.DeleteFunc(f.monitors, func(m Monitor) bool { return m.Name == Output })
	case args[0] == "dispatch":
		m := moveRE.FindStringSubmatch(args[1])
		if m == nil {
			return nil, errors.New("unexpected dispatch " + args[1])
		}
		for i, w := range f.windows {
			if w.Address == m[2] {
				f.windows[i].Workspace = f.workspace(m[1])
			}
		}
	}
	return []byte("ok\n"), nil
}

// workspace is the workspace a selector names.
func (f *fake) workspace(sel string) Workspace {
	for _, ws := range []Workspace{ws2, music, magic, surface} {
		if ws.Name == sel {
			return ws
		}
	}
	return Workspace{ID: 99, Name: sel}
}

// commands are the commands taken but queries, the eval chunk shortened.
func (f *fake) commands() []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, evalCmd+" ") {
			c = evalCmd
		}
		out = append(out, c)
	}
	return out
}

// evalCmd is hyprctl's command that runs a Lua chunk.
const evalCmd = "eval"

// chromeClass is Chrome's browser window class.
const chromeClass = "google-chrome"

// hold bounds a test turn's redirect.
const hold = 17 * time.Minute

// other is the address of a window beside Chrome on its workspace.
const other = "0xk"

var (
	ws2     = Workspace{ID: 2, Name: "2"}
	music   = Workspace{ID: -95, Name: "special:music"}
	magic   = Workspace{ID: -97, Name: "special:magic"}
	laptop  = Monitor{ID: 0, Name: "eDP-1", Width: 1920, Height: 1200, ActiveWorkspace: ws2, Focused: true}
	chrome  = Window{Address: "0xc", Class: chromeClass, Workspace: ws2, FocusHistoryID: 4}
	claudeW = Window{Address: "0xd", Class: "com.anthropic.Claude", Workspace: magic}
)

const (
	toSurface = `dispatch hl.dsp.window.move({ workspace = "name:beekeeper-browse", follow = false, window = "address:0xc" })`
	toWS2     = `dispatch hl.dsp.window.move({ workspace = "2", follow = false, window = "address:0xc" })`
	create    = "output create headless " + Output
	remove    = "output remove " + Output
)

func with(w Window, f func(*Window)) Window { f(&w); return w }

func underMagic() Monitor {
	m := laptop
	m.SpecialWorkspace = magic
	return m
}

func TestWhere(t *testing.T) {
	for name, tc := range map[string]struct {
		l    layout
		w    Window
		want string
	}{
		"on the shown workspace": {layout{monitors: []Monitor{laptop}}, chrome, ""},
		"on a special workspace not shown": {layout{monitors: []Monitor{laptop}},
			with(chrome, func(w *Window) { w.Workspace = music }), "workspace special:music, not shown on any monitor"},
		"on a workspace not shown": {layout{monitors: []Monitor{laptop}},
			with(chrome, func(w *Window) { w.Workspace = Workspace{ID: 4, Name: "4"} }), "workspace 4, not shown on any monitor"},
		"under a special workspace shown over its own": {layout{monitors: []Monitor{underMagic()}}, chrome,
			"workspace 2, under the special workspace special:magic shown over it on eDP-1"},
		"on the shown special workspace": {layout{monitors: []Monitor{underMagic()}},
			with(chrome, func(w *Window) { w.Workspace = magic; w.Floating = true }), ""},
		"on the off-screen output": {layout{monitors: []Monitor{underMagic(), {ID: 1, Name: Output, ActiveWorkspace: surface}}},
			with(chrome, func(w *Window) { w.Workspace = surface }), ""},
		"behind the front window of a monocle workspace": {
			layout{monitors: []Monitor{laptop}, monocle: true, windows: []Window{chrome, with(chrome, func(w *Window) { w.Address, w.Class, w.FocusHistoryID = other, "kitty", 1 })}},
			chrome, "workspace 2, behind its front window kitty (monocle layout)"},
		"the front window of a monocle workspace": {
			layout{monitors: []Monitor{laptop}, monocle: true, windows: []Window{chrome, with(chrome, func(w *Window) { w.Address, w.FocusHistoryID = other, 9 })}},
			chrome, ""},
		"tiled beside another": {
			layout{monitors: []Monitor{laptop}, windows: []Window{chrome, with(chrome, func(w *Window) { w.Address, w.FocusHistoryID = other, 1 })}},
			chrome, ""},
		"behind a fullscreen window": {
			layout{monitors: []Monitor{laptop}, windows: []Window{chrome, with(chrome, func(w *Window) { w.Address, w.Class, w.Fullscreen = "0xm", "mpv", 2 })}},
			chrome, "workspace 2, behind the fullscreen window mpv"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.l.where(tc.w); got != tc.want {
				t.Errorf("where = %q, want %q", got, tc.want)
			}
		})
	}
}

// Chrome under the special workspace the person works in is drawn on the
// off-screen output for the turn and put back after: nothing floats, the
// person's monitor, its special workspace and the focus stay as they were,
// and the output goes again.
func TestShowUnderASpecialWorkspace(t *testing.T) {
	f := &fake{windows: []Window{chrome, claudeW}, monitors: []Monitor{underMagic()}, layout: "monocle"}
	s, err := Show(t.Context(), f.ctl, hold)
	if err != nil || s == nil {
		t.Fatalf("Show = %v, %v", s, err)
	}
	if !s.Created || !s.Moved || !f.redirect || !strings.Contains(s.Where, "under the special workspace special:magic") {
		t.Errorf("shown = %+v, redirect %v", s, f.redirect)
	}
	if want := []string{evalCmd, create, evalCmd, toSurface}; !slices.Equal(f.commands(), want) {
		t.Errorf("commands =\n%s\nwant\n%s", strings.Join(f.commands(), "\n"), strings.Join(want, "\n"))
	}
	if f.monitors[0] != underMagic() || f.windows[0].Workspace.Name != Surface {
		t.Errorf("after Show: the person's monitor %+v, Chrome on %+v", f.monitors[0], f.windows[0].Workspace)
	}
	f.calls = nil
	if err := s.Restore(t.Context(), f.ctl); err != nil || s.Left != "" {
		t.Fatalf("Restore = %v, left %q", err, s.Left)
	}
	if want := []string{evalCmd, toWS2, remove}; !slices.Equal(f.commands(), want) {
		t.Errorf("restore commands = %q, want %q", f.commands(), want)
	}
	if len(f.monitors) != 1 || f.monitors[0] != underMagic() || f.windows[0] != chrome || f.redirect {
		t.Errorf("after Restore: monitors %+v, Chrome %+v, redirect %v", f.monitors, f.windows[0], f.redirect)
	}
	f.calls = nil
	if err := s.Restore(t.Context(), f.ctl); err != nil || !slices.Equal(f.commands(), []string{evalCmd}) || f.windows[0] != chrome {
		t.Errorf("a second restore = %v, ran %q", err, f.calls)
	}
}

// The rules give the output its far-off place and its numbered workspace
// before it exists, in Lua; a classic configuration takes them as
// keywords, but has no window rule a turn could turn off again: Show
// refuses it, the output removed and the window left where it is.
func TestShowClassicSyntax(t *testing.T) {
	f := &fake{windows: []Window{with(chrome, func(w *Window) { w.Workspace = music })}, monitors: []Monitor{laptop}, classic: true}
	s, err := Show(t.Context(), f.ctl, hold)
	if s != nil || err == nil || !strings.Contains(err.Error(), "a Lua configuration's window rule") {
		t.Fatalf("Show = %+v, %v; want the redirect refused", s, err)
	}
	for _, want := range []string{
		"keyword monitor BEEKEEPER-BROWSE,1280x800,-30000x-30000,1",
		"keyword workspace name:beekeeper-browse,default:false",
		"keyword workspace 31337,monitor:BEEKEEPER-BROWSE,default:true,defaultName:beekeeper-browse",
		create,
		remove,
	} {
		if !slices.Contains(f.calls, want) {
			t.Errorf("commands %q lack %q", f.calls, want)
		}
	}
	if f.windows[0].Workspace != music || len(f.monitors) != 1 {
		t.Errorf("Chrome on %+v, monitors %+v", f.windows[0].Workspace, f.monitors)
	}
	for _, want := range []string{`hl.monitor({ output = "BEEKEEPER-BROWSE", mode = "1280x800", position = "-30000x-30000", scale = "1" })`,
		`hl.workspace_rule({ workspace = "name:beekeeper-browse", default = false })`,
		`hl.workspace_rule({ workspace = "31337", monitor = "BEEKEEPER-BROWSE", default = true, default_name = "beekeeper-browse" })`} {
		if !strings.Contains(f.calls[0], want) {
			t.Errorf("eval %q lacks %q", f.calls[0], want)
		}
	}
}

// The redirect is a rule on the browser classes alone that maps a window
// on the output's workspace silently, without the focus and deaf to
// activation, ended by a timer at hold; turning it off ends the timer too.
func TestRedirect(t *testing.T) {
	on := redirectOn(hold)
	for _, want := range []string{
		`match = { class = "^(google-chrome|google-chrome-beta|google-chrome-unstable|chromium|chromium-browser)$" }`,
		`workspace = "name:beekeeper-browse silent"`,
		"no_initial_focus = true",
		`suppress_event = "activate activatefocus fullscreen maximize"`,
		"r:set_enabled(true)",
		`timeout = 1020000, type = "oneshot"`,
	} {
		if !strings.Contains(on, want) {
			t.Errorf("redirectOn = %q lacks %q", on, want)
		}
	}
	if !strings.HasPrefix(on, redirectOff) || !strings.Contains(redirectOff, "beekeeper_browse_timer:set_enabled(false)") {
		t.Errorf("redirectOn %q does not end an earlier turn's redirect first", on)
	}
	if re := regexp.MustCompile(browserPattern()); re.MatchString("chrome-tidal.com__-Default") || !re.MatchString(chromeClass) {
		t.Errorf("browserPattern %q matches an app window or misses Chrome", browserPattern())
	}
}

// A pop-up the turn's page opens lands on the output, never on the
// person's workspace, Chrome shown on it or not, and goes with Chrome's
// workspace after; once the turn ended a window maps as the person's rules
// say again.
func TestAPopUpDuringTheTurnLandsOnTheOutput(t *testing.T) {
	for name, start := range map[string]Window{"Chrome shown": chrome, "Chrome not shown": with(chrome, func(w *Window) { w.Workspace = music })} {
		t.Run(name, func(t *testing.T) {
			f := &fake{windows: []Window{start}, monitors: []Monitor{laptop}}
			s, err := Show(t.Context(), f.ctl, hold)
			if err != nil || s == nil || s.Moved != (start.Workspace == music) {
				t.Fatalf("Show = %+v, %v", s, err)
			}
			f.open(Window{Address: "0xe", Class: chromeClass, Floating: true})
			if got := f.windows[1].Workspace.Name; got != Surface || f.monitors[0] != laptop {
				t.Fatalf("the pop-up on %s, the person's monitor %+v", got, f.monitors[0])
			}
			if err := s.Restore(t.Context(), f.ctl); err != nil || s.Opened != 1 || s.Left != "" || len(f.monitors) != 1 {
				t.Fatalf("Restore = %v, %+v, monitors %+v", err, s, f.monitors)
			}
			if f.windows[0] != start || f.windows[1].Workspace != start.Workspace {
				t.Errorf("after Restore: Chrome %+v, the pop-up on %+v", f.windows[0], f.windows[1].Workspace)
			}
			f.open(Window{Address: "0xf", Class: chromeClass})
			if f.windows[2].Workspace != ws2 {
				t.Errorf("a window after the turn mapped on %+v", f.windows[2].Workspace)
			}
		})
	}
}

// An output that would change the person's view, or take a numbered
// workspace, is removed again before Chrome moves: their monitors,
// workspace numbers and focus stay theirs.
func TestShowRefusesAnOutputThatChangesThePersonsView(t *testing.T) {
	for name, tc := range map[string]struct {
		onCreate func(f *fake, out *Monitor)
		says     string
	}{
		"a numbered workspace": {func(_ *fake, out *Monitor) { out.ActiveWorkspace = Workspace{ID: 5, Name: "5"} }, "shows workspace 5, not beekeeper-browse"},
		"the focus moved": {func(f *fake, out *Monitor) {
			f.monitors[0].Focused, out.Focused = false, true
		}, "changed the person's monitors"},
		"the person's monitor moved": {func(f *fake, _ *Monitor) { f.monitors[0].X = 1280 }, "changed the person's monitors"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fake{windows: []Window{chrome}, monitors: []Monitor{underMagic()}, onCreate: tc.onCreate}
			s, err := Show(t.Context(), f.ctl, hold)
			if s != nil || err == nil || !strings.Contains(err.Error(), tc.says) || !strings.Contains(err.Error(), "under the special workspace special:magic") {
				t.Fatalf("Show = %+v, %v; want an error saying %q", s, err, tc.says)
			}
			if want := []string{evalCmd, create, evalCmd, remove}; !slices.Equal(f.commands(), want) {
				t.Errorf("commands = %q, want %q", f.commands(), want)
			}
			if f.windows[0] != chrome {
				t.Errorf("Chrome moved: %+v", f.windows[0])
			}
		})
	}
}

// An output there already is used, not created again, and removed after:
// it is beekeeper's by its name.
func TestShowOnTheOutputThereAlready(t *testing.T) {
	out := Monitor{ID: 1, Name: Output, ActiveWorkspace: surface}
	f := &fake{windows: []Window{with(chrome, func(w *Window) { w.Workspace = music })}, monitors: []Monitor{laptop, out}}
	s, err := Show(t.Context(), f.ctl, hold)
	if err != nil || s == nil || s.Created {
		t.Fatalf("Show = %+v, %v", s, err)
	}
	if !slices.Equal(f.commands(), []string{evalCmd, toSurface}) {
		t.Errorf("commands = %q", f.commands())
	}
	f.calls = nil
	if err := s.Restore(t.Context(), f.ctl); err != nil || len(f.monitors) != 1 {
		t.Fatalf("Restore = %v, monitors %+v", err, f.monitors)
	}
	want := []string{evalCmd, `dispatch hl.dsp.window.move({ workspace = "special:music", follow = false, window = "address:0xc" })`, remove}
	if !slices.Equal(f.commands(), want) {
		t.Errorf("restore commands = %q, want %q", f.commands(), want)
	}
}

func TestShowNoBrowserWindow(t *testing.T) {
	f := &fake{windows: []Window{claudeW, with(chrome, func(w *Window) { w.Class, w.Workspace = "chrome-tidal.com__-Default", music })}, monitors: []Monitor{laptop}}
	s, err := Show(t.Context(), f.ctl, hold)
	if s != nil || err != nil || len(f.calls) != 0 {
		t.Errorf("Show = %+v, %v, ran %q", s, err, f.calls)
	}
	if err := s.Restore(t.Context(), f.ctl); err != nil {
		t.Errorf("Restore of nothing shown = %v", err)
	}
}

// Chrome shown on the person's screen stays where it is; the output and
// the redirect are readied all the same, for the windows it opens.
func TestShowChromeShown(t *testing.T) {
	f := &fake{windows: []Window{chrome}, monitors: []Monitor{laptop}}
	s, err := Show(t.Context(), f.ctl, hold)
	if err != nil || s == nil || s.Moved || s.Where != "" || !f.redirect {
		t.Fatalf("Show = %+v, %v, redirect %v", s, err, f.redirect)
	}
	if want := []string{evalCmd, create, evalCmd}; !slices.Equal(f.commands(), want) || f.windows[0] != chrome {
		t.Errorf("commands = %q, Chrome %+v", f.commands(), f.windows[0])
	}
}

// The most recently focused browser window is the one the extension opens
// its tabs in.
func TestBrowserIsTheLastFocused(t *testing.T) {
	older := with(chrome, func(w *Window) { w.Address, w.FocusHistoryID = "0xo", 21 })
	l := layout{windows: []Window{older, claudeW, chrome}}
	if b := l.browser(); b == nil || b.Address != "0xc" {
		t.Errorf("browser = %+v", b)
	}
}

// A move that fails is undone by removing the output it created, and the
// error names where the window is.
func TestShowFailsNamingTheWorkspace(t *testing.T) {
	f := &fake{windows: []Window{with(chrome, func(w *Window) { w.Workspace = music })}, monitors: []Monitor{laptop}, refuse: []string{"address:0xc"}}
	s, err := Show(t.Context(), f.ctl, hold)
	if s != nil || err == nil {
		t.Fatalf("Show = %+v, %v; want an error", s, err)
	}
	for _, want := range []string{"workspace special:music, not shown on any monitor", "readying the off-screen output BEEKEEPER-BROWSE failed", "movetoworkspacesilent", "error: refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "more") {
		t.Errorf("error %q carries more than hyprctl's first line", err)
	}
	if c := f.commands(); c[len(c)-1] != remove || len(f.monitors) != 1 {
		t.Errorf("commands = %q, monitors %+v: want the output removed", c, f.monitors)
	}
}

// Every window on the output's workspace goes to Chrome's workspace before
// the output is removed, so none lands on the person's screen; one that
// does not move keeps the output.
func TestRestoreMovesEveryWindowOffTheOutput(t *testing.T) {
	f := &fake{windows: []Window{chrome, claudeW}, monitors: []Monitor{underMagic()}}
	s, err := Show(t.Context(), f.ctl, hold)
	if err != nil || s == nil {
		t.Fatalf("Show = %+v, %v", s, err)
	}
	popup := Window{Address: "0xe", Class: chromeClass, Workspace: surface}
	f.windows = append(f.windows, popup)
	f.calls, f.refuse = nil, []string{"address:0xe"}
	if err := s.Restore(t.Context(), f.ctl); err == nil || !strings.Contains(err.Error(), "the output BEEKEEPER-BROWSE stays") {
		t.Fatalf("Restore with a window stuck on the output = %v", err)
	}
	if len(f.monitors) != 2 || slices.Contains(f.calls, remove) {
		t.Errorf("the output was removed with a window on it: %q", f.calls)
	}
	f.calls, f.refuse = nil, nil
	if err := s.Restore(t.Context(), f.ctl); err != nil || len(f.monitors) != 1 || s.Opened != 1 {
		t.Fatalf("Restore = %v, monitors %+v, opened %d", err, f.monitors, s.Opened)
	}
	for _, w := range f.windows {
		if w.Address != claudeW.Address && w.Workspace != ws2 {
			t.Errorf("window %s on %+v, want workspace 2", w.Address, w.Workspace)
		}
	}
}

// A window the person moved off the output, or closed, stays where it is;
// the output goes all the same.
func TestRestoreLeavesAWindowThePersonMoved(t *testing.T) {
	f := &fake{windows: []Window{with(chrome, func(w *Window) { w.Workspace = music })}, monitors: []Monitor{laptop}}
	s, err := Show(t.Context(), f.ctl, hold)
	if err != nil || s == nil {
		t.Fatalf("Show = %+v, %v", s, err)
	}
	f.calls = nil
	f.windows[0].Workspace = Workspace{ID: 5, Name: "5"}
	if err := s.Restore(t.Context(), f.ctl); err != nil || !slices.Equal(f.commands(), []string{evalCmd, remove}) || s.Left != "moved by the person to workspace 5" {
		t.Errorf("Restore of a moved window = %v, left %q, ran %q", err, s.Left, f.calls)
	}
	f.calls, f.windows = nil, nil
	if err := s.Restore(t.Context(), f.ctl); err != nil || !slices.Equal(f.commands(), []string{evalCmd}) || s.Left != "closed" {
		t.Errorf("Restore of a closed window = %v, left %q, ran %q", err, s.Left, f.calls)
	}
}

// A placement survives as JSON: a browse that died mid-turn is put back by
// the next one from its record.
func TestShownRoundTrips(t *testing.T) {
	f := &fake{windows: []Window{chrome}, monitors: []Monitor{underMagic()}}
	s, err := Show(t.Context(), f.ctl, hold)
	if err != nil || s == nil {
		t.Fatalf("Show = %+v, %v", s, err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back Shown
	if err := json.Unmarshal(b, &back); err != nil || back.Before != s.Before || back.Where != s.Where || !back.Created || !back.Moved {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	if err := back.Restore(t.Context(), f.ctl); err != nil || f.windows[0] != chrome || len(f.monitors) != 1 || f.redirect {
		t.Errorf("Restore from the record = %v, Chrome %+v, monitors %+v", err, f.windows[0], f.monitors)
	}
}

func TestSelector(t *testing.T) {
	for ws, want := range map[Workspace]string{ws2: "2", music: "special:music", {ID: 12, Name: "mail"}: "name:mail", surface: "name:beekeeper-browse"} {
		if got := ws.selector(); got != want {
			t.Errorf("selector(%+v) = %q, want %q", ws, got, want)
		}
	}
}

func TestInView(t *testing.T) {
	onMusic := with(chrome, func(w *Window) { w.Workspace = music })
	for name, tc := range map[string]struct {
		windows []Window
		monitor Monitor
		want    string
	}{
		"focused":                   {[]Window{with(onMusic, func(w *Window) { w.FocusHistoryID = 0 })}, laptop, "a Chrome window has the focus"},
		"focused just before":       {[]Window{with(onMusic, func(w *Window) { w.FocusHistoryID = 1 })}, laptop, "a Chrome window had the focus before the current window"},
		"shown":                     {[]Window{chrome}, laptop, "a Chrome window is on workspace 2, which eDP-1 shows"},
		"under a special workspace": {[]Window{chrome}, underMagic(), "a Chrome window is on workspace 2, which eDP-1 shows"},
		"on the special workspace":  {[]Window{with(chrome, func(w *Window) { w.Workspace = magic })}, underMagic(), "a Chrome window is on workspace special:magic, which eDP-1 shows"},
		"hidden":                    {[]Window{onMusic}, laptop, ""},
		"a web app's window":        {[]Window{with(onMusic, func(w *Window) { w.Class, w.FocusHistoryID = "chrome-meet.google.com__-Default", 0 })}, laptop, ""},
		"the turn's, on the output": {[]Window{with(chrome, func(w *Window) { w.Workspace, w.FocusHistoryID = surface, 0 })}, laptop, ""},
	} {
		t.Run(name, func(t *testing.T) {
			l := layout{windows: tc.windows, monitors: []Monitor{tc.monitor, {ID: 1, Name: Output, ActiveWorkspace: surface}}}
			if got := l.inView(); got != tc.want {
				t.Errorf("inView = %q, want %q", got, tc.want)
			}
		})
	}
}

// While the person types the redirect is off, so a window they open stays
// on their screen; on again, a window Chrome opens is drawn on the output.
// Each change is made once, and Restore ends it.
func TestRedirectFollowsThePerson(t *testing.T) {
	f := &fake{windows: []Window{with(chrome, func(w *Window) { w.Workspace = music })}, monitors: []Monitor{laptop}}
	s, err := Show(t.Context(), f.ctl, hold)
	if err != nil || s == nil || !f.redirect {
		t.Fatalf("Show = %+v, %v, redirect %v", s, err, f.redirect)
	}
	f.calls = nil
	for range 2 {
		if err := s.Redirect(t.Context(), f.ctl, false, hold); err != nil {
			t.Fatal(err)
		}
	}
	if f.redirect || len(f.calls) != 1 {
		t.Fatalf("redirect off: on %v, commands %q", f.redirect, f.calls)
	}
	f.open(Window{Address: "0xa1", Class: chromeClass})
	if ws := f.windows[len(f.windows)-1].Workspace; ws != ws2 {
		t.Errorf("a window the person opened went to %+v, want their workspace 2", ws)
	}
	if err := s.Redirect(t.Context(), f.ctl, true, hold); err != nil || !f.redirect {
		t.Fatalf("redirect on = %v, on %v", err, f.redirect)
	}
	f.open(Window{Address: "0xa2", Class: chromeClass})
	if ws := f.windows[len(f.windows)-1].Workspace; ws != surface {
		t.Errorf("a window Chrome opened during a quiet turn went to %+v, want the output's", ws)
	}
	if err := s.Restore(t.Context(), f.ctl); err != nil || f.redirect {
		t.Fatalf("Restore = %v, redirect %v", err, f.redirect)
	}
	f.calls = nil
	if err := s.Redirect(t.Context(), f.ctl, false, hold); err != nil || len(f.calls) != 0 {
		t.Errorf("redirect off after Restore = %v, commands %q", err, f.calls)
	}
}

// The person turned to the shared window once a monitor of theirs shows
// its own workspace, never for the output's.
func TestTurned(t *testing.T) {
	f := &fake{windows: []Window{with(chrome, func(w *Window) { w.Workspace = music })}, monitors: []Monitor{laptop}}
	s, err := Show(t.Context(), f.ctl, hold)
	if err != nil || s == nil || !s.Moved {
		t.Fatalf("Show = %+v, %v", s, err)
	}
	if got, err := s.Turned(t.Context(), f.ctl); err != nil || got != "" {
		t.Errorf("Turned = %q, %v; want not turned", got, err)
	}
	f.monitors[0].SpecialWorkspace = music
	if got, err := s.Turned(t.Context(), f.ctl); err != nil || got != "eDP-1 shows workspace special:music, the shared Chrome window's" {
		t.Errorf("Turned = %q, %v", got, err)
	}
	f.monitors[0].SpecialWorkspace, f.monitors[0].ActiveWorkspace = Workspace{}, ws2
	if got, err := s.Turned(t.Context(), f.ctl); err != nil || got != "" {
		t.Errorf("Turned on workspace 2 = %q, %v; want not turned", got, err)
	}
	if got, err := (*Shown)(nil).Turned(t.Context(), f.ctl); err != nil || got != "" {
		t.Errorf("Turned of no placement = %q, %v", got, err)
	}
}
