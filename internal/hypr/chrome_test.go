package hypr

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// fake is a compositor: its windows and monitors answer hyprctl's queries,
// and the dispatches it takes are recorded; refuse answers each dispatch
// holding one of its words with an error text instead of ok.
type fake struct {
	windows  []Window
	monitors []Monitor
	layout   string
	calls    []string
	refuse   []string
}

func (f *fake) ctl(_ context.Context, args ...string) ([]byte, error) {
	switch {
	case slices.Equal(args, []string{"-j", "clients"}):
		return json.Marshal(f.windows)
	case slices.Equal(args, []string{"-j", "monitors"}):
		return json.Marshal(f.monitors)
	case slices.Equal(args, []string{"-j", "getoption", "general:layout"}):
		return json.Marshal(map[string]any{"option": "general:layout", "str": f.layout, "set": true})
	case len(args) == 2 && args[0] == "dispatch":
		f.calls = append(f.calls, args[1])
		for _, r := range f.refuse {
			if strings.Contains(args[1], r) {
				return []byte("error: refused\nmore"), nil
			}
		}
		return []byte("ok\n"), nil
	}
	return nil, errors.New("unexpected hyprctl " + strings.Join(args, " "))
}

// lua are the Lua calls among the dispatches taken.
func (f *fake) lua() []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "hl.dsp.") {
			out = append(out, c)
		}
	}
	return out
}

// other is the address of a window beside Chrome on its workspace.
const other = "0xk"

// unfloat is the Lua dispatch that tiles Chrome again.
const unfloat = `hl.dsp.window.float({ action = "unset", window = "address:0xc" })`

var (
	ws2     = Workspace{ID: 2, Name: "2"}
	music   = Workspace{ID: -95, Name: "special:music"}
	magic   = Workspace{ID: -97, Name: "special:magic"}
	laptop  = Monitor{ID: 0, Name: "eDP-1", Width: 1920, Height: 1200, Scale: 1, Reserved: [4]int{0, 38, 0, 0}, ActiveWorkspace: ws2, Focused: true}
	chrome  = Window{Address: "0xc", Class: "google-chrome", Workspace: ws2, At: [2]int{22, 60}, Size: [2]int{1876, 1118}, FocusHistoryID: 4}
	claudeW = Window{Address: "0xd", Class: "com.anthropic.Claude", Workspace: magic, At: [2]int{22, 60}, Size: [2]int{1876, 1118}}
)

func with(w Window, f func(*Window)) Window { f(&w); return w }

func TestWhere(t *testing.T) {
	underMagic := laptop
	underMagic.SpecialWorkspace = magic
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
		"under a special workspace shown over its own": {layout{monitors: []Monitor{underMagic}}, chrome,
			"workspace 2, under the special workspace special:magic shown over it on eDP-1"},
		"on the shown special workspace": {layout{monitors: []Monitor{underMagic}},
			with(chrome, func(w *Window) { w.Workspace = magic; w.Floating = true }), ""},
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

// Chrome under the special workspace the person works in is shown floating
// on that special workspace, the focus left alone, and put back tiled on
// its own workspace after.
func TestShowUnderASpecialWorkspace(t *testing.T) {
	m := laptop
	m.SpecialWorkspace = magic
	f := &fake{windows: []Window{chrome, claudeW}, monitors: []Monitor{m}, layout: "monocle"}
	s, err := Show(t.Context(), f.ctl)
	if err != nil || s == nil {
		t.Fatalf("Show = %v, %v", s, err)
	}
	if s.To != magic || !strings.Contains(s.Where, "under the special workspace special:magic") {
		t.Errorf("shown = %+v", s)
	}
	want := []string{
		`hl.dsp.window.move({ workspace = "special:magic", follow = false, window = "address:0xc" })`,
		`hl.dsp.window.float({ action = "set", window = "address:0xc" })`,
		`hl.dsp.window.resize({ x = 1280, y = 800, window = "address:0xc" })`,
		`hl.dsp.window.move({ x = 632, y = 392, window = "address:0xc" })`,
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("dispatches =\n%s\nwant\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	f.calls = nil
	f.windows[0].Workspace, f.windows[0].Floating = magic, true
	if err := s.Restore(t.Context(), f.ctl); err != nil {
		t.Fatal(err)
	}
	want = []string{
		unfloat,
		`hl.dsp.window.move({ workspace = "2", follow = false, window = "address:0xc" })`,
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("restore dispatches =\n%s\nwant\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	f.calls = nil
	if err := s.Restore(t.Context(), f.ctl); err != nil || len(f.calls) != 0 {
		t.Errorf("a second restore = %v, dispatched %q", err, f.calls)
	}
}

// A floating Chrome on a special workspace not shown goes back to its own
// size and place, floating still, on that special workspace.
func TestShowAFloatingWindowAndPutItBack(t *testing.T) {
	w := with(chrome, func(w *Window) {
		w.Workspace, w.Floating, w.At, w.Size = music, true, [2]int{100, 100}, [2]int{900, 700}
	})
	f := &fake{windows: []Window{w}, monitors: []Monitor{laptop}}
	s, err := Show(t.Context(), f.ctl)
	if err != nil || s == nil || s.To != ws2 {
		t.Fatalf("Show = %+v, %v", s, err)
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.Contains(c, "float(") }) {
		t.Errorf("a floating window was floated: %q", f.calls)
	}
	f.calls = nil
	f.windows[0].Workspace = ws2
	if err := s.Restore(t.Context(), f.ctl); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`hl.dsp.window.resize({ x = 900, y = 700, window = "address:0xc" })`,
		`hl.dsp.window.move({ x = 100, y = 100, window = "address:0xc" })`,
		`hl.dsp.window.move({ workspace = "special:music", follow = false, window = "address:0xc" })`,
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("restore dispatches =\n%s\nwant\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestShowNothingToDo(t *testing.T) {
	for name, f := range map[string]*fake{
		"already shown":     {windows: []Window{chrome}, monitors: []Monitor{laptop}},
		"no browser window": {windows: []Window{claudeW, with(chrome, func(w *Window) { w.Class, w.Workspace = "chrome-tidal.com__-Default", music })}, monitors: []Monitor{laptop}},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := Show(t.Context(), f.ctl)
			if s != nil || err != nil || len(f.calls) != 0 {
				t.Errorf("Show = %+v, %v, dispatched %q", s, err, f.calls)
			}
			if err := s.Restore(t.Context(), f.ctl); err != nil {
				t.Errorf("Restore of nothing shown = %v", err)
			}
		})
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

// Under a classic configuration hyprctl refuses the Lua call and takes the
// classic one.
func TestShowClassicSyntax(t *testing.T) {
	f := &fake{windows: []Window{with(chrome, func(w *Window) { w.Workspace = music })}, monitors: []Monitor{laptop}, refuse: []string{"hl.dsp."}}
	if _, err := Show(t.Context(), f.ctl); err != nil {
		t.Fatal(err)
	}
	var classic []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "hl.dsp.") {
			classic = append(classic, c)
		}
	}
	want := []string{"movetoworkspacesilent 2,address:0xc", "setfloating address:0xc", "resizewindowpixel exact 1280 800,address:0xc", "movewindowpixel exact 632 392,address:0xc"}
	if !slices.Equal(classic, want) || len(f.lua()) != len(want) {
		t.Errorf("dispatches = %q, want the classic %q each after its Lua call", f.calls, want)
	}
}

// A step that fails is undone, and the error names where the window is.
func TestShowFailsNamingTheWorkspace(t *testing.T) {
	f := &fake{windows: []Window{with(chrome, func(w *Window) { w.Workspace = music })}, monitors: []Monitor{laptop}, refuse: []string{"resize"}}
	s, err := Show(t.Context(), f.ctl)
	if s != nil || err == nil {
		t.Fatalf("Show = %+v, %v; want an error", s, err)
	}
	for _, want := range []string{"workspace special:music, not shown on any monitor", "showing it failed", "resizewindowpixel", "error: refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "more") {
		t.Errorf("error %q carries more than hyprctl's first line", err)
	}
	undo := []string{
		unfloat,
		`hl.dsp.window.move({ workspace = "special:music", follow = false, window = "address:0xc" })`,
	}
	if n := len(f.calls); n < 2 || !slices.Equal(f.calls[n-2:], undo) {
		t.Errorf("dispatches = %q, want them to end undoing the float and the move: %q", f.calls, undo)
	}
}

// A window the person moved off the shown workspace, or closed, stays
// where it is.
func TestRestoreLeavesAWindowThePersonMoved(t *testing.T) {
	f := &fake{windows: []Window{with(chrome, func(w *Window) { w.Workspace = music })}, monitors: []Monitor{laptop}}
	s, err := Show(t.Context(), f.ctl)
	if err != nil || s == nil {
		t.Fatalf("Show = %+v, %v", s, err)
	}
	f.calls = nil
	f.windows[0].Workspace = Workspace{ID: 5, Name: "5"}
	if err := s.Restore(t.Context(), f.ctl); err != nil || len(f.calls) != 0 || s.Left != "moved by the person to workspace 5" {
		t.Errorf("Restore of a moved window = %v, left %q, dispatched %q", err, s.Left, f.calls)
	}
	f.windows = nil
	if err := s.Restore(t.Context(), f.ctl); err != nil || len(f.calls) != 0 || s.Left != "closed" {
		t.Errorf("Restore of a closed window = %v, left %q, dispatched %q", err, s.Left, f.calls)
	}
}

// A window the person moved back to its own workspace while it floated
// there is only tiled again, not moved.
func TestRestoreTilesAWindowMovedHome(t *testing.T) {
	m := laptop
	m.SpecialWorkspace = magic
	f := &fake{windows: []Window{chrome, claudeW}, monitors: []Monitor{m}, layout: "monocle"}
	s, err := Show(t.Context(), f.ctl)
	if err != nil || s == nil {
		t.Fatalf("Show = %+v, %v", s, err)
	}
	f.calls = nil
	f.windows[0].Floating = true
	if err := s.Restore(t.Context(), f.ctl); err != nil || s.Left != "" {
		t.Fatalf("Restore = %v, left %q", err, s.Left)
	}
	want := []string{unfloat}
	if !slices.Equal(f.calls, want) {
		t.Errorf("dispatches = %q, want %q", f.calls, want)
	}
}

func TestCorner(t *testing.T) {
	for name, tc := range map[string]struct {
		m        Monitor
		at, size [2]int
	}{
		"a laptop with a top bar": {laptop, [2]int{632, 392}, [2]int{1280, 800}},
		"a scaled second monitor": {Monitor{X: 1920, Width: 3840, Height: 2160, Scale: 2}, [2]int{1920 + 1920 - 8 - 1280, 1080 - 8 - 800}, [2]int{1280, 800}},
		"a small screen":          {Monitor{Width: 1024, Height: 768, Scale: 1}, [2]int{8, 8}, [2]int{1008, 752}},
	} {
		t.Run(name, func(t *testing.T) {
			if at, size := corner(tc.m); at != tc.at || size != tc.size {
				t.Errorf("corner = %v %v, want %v %v", at, size, tc.at, tc.size)
			}
		})
	}
}

func TestSelector(t *testing.T) {
	for ws, want := range map[Workspace]string{ws2: "2", music: "special:music", {ID: 12, Name: "mail"}: "name:mail"} {
		if got := ws.selector(); got != want {
			t.Errorf("selector(%+v) = %q, want %q", ws, got, want)
		}
	}
}
