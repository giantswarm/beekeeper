package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/hypr"
)

// compositor answers hyprctl with Chrome on a special workspace not shown
// and records the dispatches.
type compositor struct {
	window hypr.Window
	calls  []string
}

func (c *compositor) ctl(_ context.Context, args ...string) ([]byte, error) {
	switch args[len(args)-1] {
	case "clients":
		return json.Marshal([]hypr.Window{c.window})
	case "monitors":
		return json.Marshal([]hypr.Monitor{{Name: "eDP-1", Width: 1920, Height: 1200, Scale: 1, Focused: true,
			ActiveWorkspace: hypr.Workspace{ID: 2, Name: "2"}}})
	case "general:layout":
		return []byte(`{"str":"dwindle"}`), nil
	}
	c.calls = append(c.calls, args[len(args)-1])
	if strings.Contains(args[len(args)-1], `workspace = "2"`) {
		c.window.Workspace = hypr.Workspace{ID: 2, Name: "2"}
	}
	return []byte("ok"), nil
}

func stubCompositor(t *testing.T, c *compositor) {
	t.Helper()
	running, ctl := hyprRunning, chromeCtl
	t.Cleanup(func() { hyprRunning, chromeCtl = running, ctl })
	hyprRunning, chromeCtl = func() bool { return true }, c.ctl
}

// Chrome on a special workspace not shown is shown for the turn and put
// back; the findings say where it was.
func TestShowChromeForTheTurn(t *testing.T) {
	_, _ = stubApp(t)
	plat.Machine = lockMachine{Machine: plat.Machine}
	c := &compositor{window: hypr.Window{Address: "0xc", Class: "google-chrome", Workspace: hypr.Workspace{ID: -95, Name: "special:music"}}}
	stubCompositor(t, c)
	p := showChrome(t.Context())
	if p.To != "2" || p.Locked != "" || p.Failed != "" || len(c.calls) != 4 {
		t.Fatalf("placement = %+v, dispatches %q", p, c.calls)
	}
	if err := p.restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if last := c.calls[len(c.calls)-1]; !strings.Contains(last, `workspace = "special:music"`) {
		t.Errorf("last dispatch = %q, want the move back to special:music", last)
	}
	want := "window shown: the shared Chrome window was on workspace special:music, not shown on any monitor; " +
		"the turn showed it floating on workspace 2 and put it back after"
	if got := p.line(); got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
	n := len(c.calls)
	if err := p.restore(t.Context()); err != nil || len(c.calls) != n {
		t.Errorf("a second restore = %v, dispatched %q", err, c.calls[n:])
	}
}

// Under a locked screen browse moves nothing, and a page not drawn fails
// the command as screen locked, naming the locker.
func TestShowChromeUnderALockedScreen(t *testing.T) {
	_, _ = stubApp(t)
	plat.Machine = lockMachine{Machine: plat.Machine, locker: hyprlock}
	c := &compositor{window: hypr.Window{Address: "0xc", Class: "google-chrome", Workspace: hypr.Workspace{ID: -95, Name: "special:music"}}}
	stubCompositor(t, c)
	p := showChrome(t.Context())
	if p.Locked != hyprlock || len(c.calls) != 0 {
		t.Fatalf("placement = %+v, dispatches %q", p, c.calls)
	}
	if l := p.line(); !strings.HasPrefix(l, "screen locked: hyprlock runs") {
		t.Errorf("line = %q", l)
	}
	f := browseFindings{NotDrawn: 2, Window: *p}
	if err := f.drawErr(); err == nil || !strings.HasPrefix(err.Error(), "screen locked: hyprlock runs") || !strings.Contains(err.Error(), guard.NotDrawnLine) {
		t.Errorf("drawErr = %v", err)
	}
}

// A window browse could not show is named where it is in the draw error.
func TestDrawErrNamesWhereTheWindowIs(t *testing.T) {
	failed := "the shared Chrome window is on workspace special:music, not shown on any monitor; showing it failed: no socket"
	f := browseFindings{NotDrawn: 1, Window: placement{Failed: failed}}
	if err := f.drawErr(); err == nil || !strings.HasPrefix(err.Error(), guard.NotDrawnLine+": ") || !strings.Contains(err.Error(), "workspace special:music") {
		t.Errorf("drawErr = %v", err)
	}
	if l := f.Window.line(); l != "window not shown: "+failed {
		t.Errorf("line = %q", l)
	}
	if l := (placement{}).line(); l != "" {
		t.Errorf("line of a window found shown = %q", l)
	}
}
