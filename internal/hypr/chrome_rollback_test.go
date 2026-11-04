package hypr

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// Losing the hyprctl response does not mean its command did not run. A
// cancelled setup must restore any accepted move and redirected pop-ups
// under a fresh deadline, removing only an output it created itself.
func TestShowRollsBackCancelledSetup(t *testing.T) {
	for name, tc := range map[string]struct {
		at        string
		lostReply bool
		reused    bool
	}{
		"after output creation":     {at: create},
		"after redirect activation": {at: "set_enabled(true)"},
		"after window move":         {at: toSurface},
		"lost redirect reply":       {at: "set_enabled(true)", lostReply: true},
		"lost window move reply":    {at: toSurface, lostReply: true},
		"reused output":             {at: toSurface, reused: true},
	} {
		t.Run(name, func(t *testing.T) {
			main := with(chrome, func(w *Window) { w.Workspace = music })
			f := &fake{windows: []Window{main}, monitors: []Monitor{laptop}}
			if tc.reused {
				f.monitors = append(f.monitors, Monitor{ID: 1, Name: Output, ActiveWorkspace: surface})
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fired, cleaned := false, false
			ctl := func(ctx context.Context, args ...string) ([]byte, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if fired && slices.Equal(args, []string{evalCmd, redirectOff}) {
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 10*time.Second {
						t.Errorf("rollback has no independent bounded deadline")
					}
					cleaned = true
				}
				out, err := f.ctl(ctx, args...)
				if !fired && strings.Contains(strings.Join(args, " "), tc.at) {
					fired = true
					if f.redirect {
						f.open(Window{Address: "0xe", Class: chromeClass})
					}
					if tc.lostReply {
						return nil, errors.New("lost compositor reply")
					}
					cancel()
					if tc.at == toSurface {
						return nil, context.Canceled
					}
				}
				return out, err
			}
			s, err := Show(ctx, ctl, hold)
			if s != nil || err == nil || !fired || !cleaned {
				t.Fatalf("Show = %+v, %v; fired %v, cleaned %v", s, err, fired, cleaned)
			}
			wantMonitors := 1
			if tc.reused {
				wantMonitors++
			}
			if f.redirect || len(f.monitors) != wantMonitors || f.monitors[0] != laptop || slices.Contains(f.calls, remove) == tc.reused {
				t.Errorf("after rollback: redirect %v, monitors %+v, commands %q", f.redirect, f.monitors, f.commands())
			}
			for _, w := range f.windows {
				if w.Workspace != music {
					t.Errorf("window %s stayed on %+v", w.Address, w.Workspace)
				}
			}
		})
	}
}

// If returning a window or stopping the redirect fails, removing the
// output could expose that window or strand the next one on a hidden
// workspace. Keep the output and report the incomplete rollback.
func TestShowKeepsOutputWhenRollbackFails(t *testing.T) {
	for name, refuse := range map[string]string{
		"returning a window": music.Name,
		"stopping redirect":  redirectOff,
	} {
		t.Run(name, func(t *testing.T) {
			f := &fake{windows: []Window{with(chrome, func(w *Window) { w.Workspace = music })}, monitors: []Monitor{laptop}}
			fired := false
			ctl := func(ctx context.Context, args ...string) ([]byte, error) {
				out, err := f.ctl(ctx, args...)
				if !fired && strings.Join(args, " ") == toSurface {
					fired, f.refuse = true, []string{refuse}
					return nil, errors.New("lost compositor reply")
				}
				return out, err
			}
			s, err := Show(t.Context(), ctl, hold)
			if s != nil || err == nil || !strings.Contains(err.Error(), "the output "+Output+" stays") {
				t.Fatalf("Show = %+v, %v; want incomplete rollback", s, err)
			}
			if len(f.monitors) != 2 || f.windows[0].Workspace != surface || slices.Contains(f.calls, remove) {
				t.Errorf("failed rollback removed its surface: monitors %+v, windows %+v, commands %q", f.monitors, f.windows, f.commands())
			}
		})
	}
}
