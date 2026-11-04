package hypr

import (
	"encoding/json"
	"strings"
	"testing"
)

// A failed redirect shutdown cannot advance restoration: new windows can
// still map onto the surface until the stop succeeds. This also applies
// to a placement recovered from JSON, without its private redirect flag.
func TestRestoreWaitsForRedirectShutdown(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		name := "same turn"
		if recovered {
			name = "recovered turn"
		}
		t.Run(name, func(t *testing.T) {
			f := &fake{windows: []Window{with(chrome, func(w *Window) { w.Workspace = music })}, monitors: []Monitor{laptop}}
			s, err := Show(t.Context(), f.ctl, hold)
			if err != nil || s == nil {
				t.Fatalf("Show = %+v, %v", s, err)
			}
			if recovered {
				b, err := json.Marshal(s)
				if err != nil {
					t.Fatal(err)
				}
				s = &Shown{}
				if err := json.Unmarshal(b, s); err != nil {
					t.Fatal(err)
				}
			}
			redirecting := s.redirecting
			f.calls, f.refuse = nil, []string{redirectOff}
			if err := s.Restore(t.Context(), f.ctl); err == nil || !strings.Contains(err.Error(), "the Chrome redirect is stopped") {
				t.Fatalf("Restore = %v; want a failed redirect shutdown", err)
			}
			if len(f.calls) != 1 || !f.redirect || s.redirecting != redirecting || len(f.monitors) != 2 || f.windows[0].Workspace != surface {
				t.Fatalf("failed Restore changed placement: %+v, monitors %+v, redirect %v, commands %q", s, f.monitors, f.redirect, f.commands())
			}
			// A new pop-up still maps on the retained output. The next
			// successful restore must return both windows to their workspace.
			f.open(Window{Address: "0xe", Class: chromeClass})
			f.refuse = nil
			if !recovered {
				if err := s.Redirect(t.Context(), f.ctl, false, hold); err != nil || f.redirect {
					t.Fatalf("retry Redirect off = %v, active %v", err, f.redirect)
				}
			}
			if err := s.Restore(t.Context(), f.ctl); err != nil || f.redirect || len(f.monitors) != 1 || s.Left != "" || s.Opened != 1 {
				t.Fatalf("retry Restore = %v, placement %+v, monitors %+v, redirect %v", err, s, f.monitors, f.redirect)
			}
			for _, w := range f.windows {
				if w.Workspace != music {
					t.Errorf("window %s stayed on %+v", w.Address, w.Workspace)
				}
			}
		})
	}
}
