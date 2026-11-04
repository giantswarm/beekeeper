package hypr

import "testing"

// An unfocused pop-up must not take the shared window's place: the
// extension's tabs still open in the browser with the most recent focus.
func TestShowPrefersAFocusedBrowserToAnUnfocusedPopUp(t *testing.T) {
	main := with(chrome, func(w *Window) { w.Workspace = music })
	popup := with(chrome, func(w *Window) { w.Address, w.FocusHistoryID = "0xe", -1 })
	for name, windows := range map[string][]Window{
		"pop-up first": {popup, main},
		"pop-up last":  {main, popup},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fake{windows: windows, monitors: []Monitor{laptop}}
			s, err := Show(t.Context(), f.ctl, hold)
			if err != nil || s == nil {
				t.Fatalf("Show = %+v, %v", s, err)
			}
			if s.Before != main || !s.Moved {
				t.Fatalf("selected %+v, moved %v; want the last-focused browser %+v", s.Before, s.Moved, main)
			}
			for _, w := range f.windows {
				if w.Address == main.Address && w.Workspace != surface || w.Address == popup.Address && w != popup {
					t.Errorf("after Show: window %+v", w)
				}
			}
			if err := s.Restore(t.Context(), f.ctl); err != nil {
				t.Fatal(err)
			}
			for _, w := range f.windows {
				if w != main && w != popup {
					t.Errorf("after Restore: unexpected window %+v", w)
				}
			}
		})
	}
}

func TestBrowserWithoutFocusHistory(t *testing.T) {
	unfocused := with(chrome, func(w *Window) { w.FocusHistoryID = -1 })
	second := with(unfocused, func(w *Window) { w.Address = "0xe" })
	l := layout{windows: []Window{claudeW, unfocused, second}}
	if got := l.browser(); got == nil || *got != unfocused {
		t.Errorf("browser = %+v; want the first browser when none has a focus history", got)
	}
}
