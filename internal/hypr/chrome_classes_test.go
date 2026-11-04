package hypr

import "testing"

// Selection accepts the XWayland class casing too. Its pop-ups must use
// the same surface as Wayland's lowercase classes, while installed apps
// and windows opened after the turn retain their normal placement.
func TestRedirectKeepsBrowserClassCasingOnTheSurface(t *testing.T) {
	for _, class := range []string{chromeClass, "Google-chrome", "Chromium", "GOOGLE-CHROME-BETA", "Chromium-browser"} {
		t.Run(class, func(t *testing.T) {
			main := with(chrome, func(w *Window) { w.Class, w.Workspace = class, music })
			f := &fake{windows: []Window{main}, monitors: []Monitor{laptop}}
			s, err := Show(t.Context(), f.ctl, hold)
			if err != nil || s == nil || !s.Moved {
				t.Fatalf("Show = %+v, %v", s, err)
			}
			f.open(Window{Address: "0xe", Class: class})
			f.open(Window{Address: "0xf", Class: "chrome-tidal.com__-Default"})
			if f.windows[1].Workspace != surface || f.windows[2].Workspace != ws2 || f.monitors[0] != laptop {
				t.Fatalf("during turn: windows %+v, monitor %+v", f.windows, f.monitors[0])
			}
			if err := s.Restore(t.Context(), f.ctl); err != nil || s.Opened != 1 || f.redirect {
				t.Fatalf("Restore = %v, placement %+v, redirect %v", err, s, f.redirect)
			}
			if f.windows[0] != main || f.windows[1].Workspace != music || f.windows[2].Workspace != ws2 {
				t.Errorf("after Restore: windows %+v", f.windows)
			}
			f.open(Window{Address: "0xa1", Class: class})
			if f.windows[3].Workspace != ws2 {
				t.Errorf("a window opened after the turn went to %+v", f.windows[3].Workspace)
			}
		})
	}
}
