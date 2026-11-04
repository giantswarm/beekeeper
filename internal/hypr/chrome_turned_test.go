package hypr

import "testing"

// A person can bring the shared window to a different workspace or show
// its surface on their monitor. The watch must yield there too, while
// ignoring unrelated windows and the headless output itself.
func TestTurnedFollowsTheSharedWindowsCurrentWorkspace(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*fake)
		want   string
	}{
		"moved to shown numbered workspace": {
			func(f *fake) { f.windows[0].Workspace = ws2 },
			"eDP-1 shows workspace 2, the shared Chrome window's",
		},
		"moved to shown special workspace": {
			func(f *fake) { f.windows[0].Workspace, f.monitors[0].SpecialWorkspace = magic, magic },
			"eDP-1 shows workspace special:magic, the shared Chrome window's",
		},
		"surface shown on a persons monitor": {
			func(f *fake) { f.monitors[0].ActiveWorkspace = surface },
			"eDP-1 shows workspace beekeeper-browse, the shared Chrome window's",
		},
		"only the off-screen output shows it": {func(*fake) {}, ""},
		"moved to another hidden workspace": {
			func(f *fake) { f.windows[0].Workspace = magic }, "",
		},
		"another browser is shown": {
			func(f *fake) { f.windows = append(f.windows, with(chrome, func(w *Window) { w.Address = "0xe" })) }, "",
		},
		"shared window closed": {func(f *fake) { f.windows = nil }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fake{windows: []Window{with(chrome, func(w *Window) { w.Workspace = music })}, monitors: []Monitor{laptop}}
			s, err := Show(t.Context(), f.ctl, hold)
			if err != nil || s == nil || !s.Moved {
				t.Fatalf("Show = %+v, %v", s, err)
			}
			tc.change(f)
			if got, err := s.Turned(t.Context(), f.ctl); err != nil || got != tc.want {
				t.Errorf("Turned = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
