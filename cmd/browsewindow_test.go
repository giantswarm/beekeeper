package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/hypr"
)

// compositor answers hyprctl with Chrome on a special workspace not shown,
// creates and removes the off-screen output, and records the commands.
type compositor struct {
	window hypr.Window
	output bool
	calls  []string
}

func (c *compositor) ctl(_ context.Context, args ...string) ([]byte, error) {
	switch args[len(args)-1] {
	case "clients":
		return json.Marshal([]hypr.Window{c.window})
	case "monitors":
		ms := []hypr.Monitor{{Name: "eDP-1", Width: 1920, Height: 1200, Focused: true, ActiveWorkspace: hypr.Workspace{ID: 2, Name: "2"}}}
		if c.output {
			ms = append(ms, hypr.Monitor{ID: 1, Name: hypr.Output, ActiveWorkspace: hypr.Workspace{ID: -1337, Name: hypr.Surface}})
		}
		return json.Marshal(ms)
	case "general:layout":
		return []byte(`{"str":"dwindle"}`), nil
	}
	call := strings.Join(args, " ")
	c.calls = append(c.calls, call)
	switch {
	case call == "output create headless "+hypr.Output:
		c.output = true
	case call == "output remove "+hypr.Output:
		c.output = false
	case strings.Contains(call, `workspace = "name:`+hypr.Surface+`"`):
		c.window.Workspace = hypr.Workspace{ID: -1337, Name: hypr.Surface}
	case strings.Contains(call, `workspace = "`+musicWS.Name+`"`):
		c.window.Workspace = musicWS
	}
	return []byte("ok"), nil
}

// musicWS is the special workspace Chrome starts on, not shown.
var musicWS = hypr.Workspace{ID: -95, Name: "special:music"}

// chromeOnMusic is a compositor with Chrome on special:music, not shown.
func chromeOnMusic() *compositor {
	return &compositor{window: hypr.Window{Address: "0xc", Class: "google-chrome", Workspace: musicWS}}
}

func stubCompositor(t *testing.T, c *compositor) {
	t.Helper()
	running, ctl := hyprRunning, chromeCtl
	t.Cleanup(func() { hyprRunning, chromeCtl = running, ctl })
	hyprRunning, chromeCtl = func() bool { return true }, c.ctl
}

// Chrome on a special workspace not shown is drawn on the off-screen
// output for the turn, recorded while it is there, and put back; the
// findings say where it was.
func TestShowChromeForTheTurn(t *testing.T) {
	_, _ = stubApp(t)
	lockScreen(t, "")
	c := chromeOnMusic()
	stubCompositor(t, c)
	record := filepath.Join(t.TempDir(), "browse", "window.json")
	p := showChrome(t.Context(), record, time.Hour)
	if p.To != hypr.Output || p.Locked != "" || p.Failed != "" || c.window.Workspace.Name != hypr.Surface {
		t.Fatalf("placement = %+v, commands %q", p, c.calls)
	}
	if _, err := os.Stat(record); err != nil {
		t.Errorf("no record while the window is on the output: %v", err)
	}
	if err := p.restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c.window.Workspace != musicWS || c.output {
		t.Errorf("after restore: Chrome on %+v, output there %v; commands %q", c.window.Workspace, c.output, c.calls)
	}
	if _, err := os.Stat(record); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the record outlived the restore: %v", err)
	}
	want := "window shown: the shared Chrome window was on workspace special:music, not shown on any monitor; " +
		"the turn drew it on the off-screen output BEEKEEPER-BROWSE, never on the person's screen, and put it back after"
	if got := p.line(); got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
	n := len(c.calls)
	if err := p.restore(t.Context()); err != nil || len(c.calls) != n {
		t.Errorf("a second restore = %v, ran %q", err, c.calls[n:])
	}
}

// A window a died turn left on the output is put back from its record by
// the next browse before its own placement.
func TestShowChromeRecoversADiedTurn(t *testing.T) {
	_, _ = stubApp(t)
	lockScreen(t, "")
	c := chromeOnMusic()
	stubCompositor(t, c)
	record := filepath.Join(t.TempDir(), "browse", "window.json")
	if died := showChrome(t.Context(), record, time.Hour); died.To == "" {
		t.Fatalf("placement = %+v", died)
	}
	p := showChrome(t.Context(), record, time.Hour)
	if p.Recovered != musicWS.Name || p.Failed != "" || p.To != hypr.Output {
		t.Fatalf("placement after a died turn = %+v, commands %q", p, c.calls)
	}
	if l := p.line(); !strings.HasPrefix(l, "window put back: a died turn had left the shared Chrome window on the off-screen output BEEKEEPER-BROWSE; "+
		"it went back to workspace special:music\nwindow shown: ") {
		t.Errorf("line = %q", l)
	}
	if err := p.restore(t.Context()); err != nil || c.window.Workspace != musicWS || c.output {
		t.Errorf("restore = %v, Chrome on %+v, output %v", err, c.window.Workspace, c.output)
	}
}

// Under a locked screen browse moves nothing, and a page not drawn fails
// the command as screen locked, naming the locker.
func TestShowChromeUnderALockedScreen(t *testing.T) {
	_, _ = stubApp(t)
	lockScreen(t, hyprlock)
	c := chromeOnMusic()
	stubCompositor(t, c)
	p := showChrome(t.Context(), filepath.Join(t.TempDir(), "window.json"), time.Hour)
	if p.Locked != hyprlock || len(c.calls) != 0 {
		t.Fatalf("placement = %+v, commands %q", p, c.calls)
	}
	if l := p.line(); !strings.HasPrefix(l, "screen locked: hyprlock runs") {
		t.Errorf("line = %q", l)
	}
	f := browseFindings{NotDrawn: 2, Window: *p}
	if err := f.drawErr(); err == nil || !strings.HasPrefix(err.Error(), "screen locked: hyprlock runs") || !strings.Contains(err.Error(), guard.NotDrawnLine) {
		t.Errorf("drawErr = %v", err)
	}
}

// Under a locked screen browse starts no turn: it fails at once as screen
// locked, naming the locker and the probe to park on; unlocked, it refuses
// nothing.
func TestBrowseRefusesALockedScreen(t *testing.T) {
	a, out := stubApp(t)
	bin := t.TempDir()
	ran := filepath.Join(bin, "ran")
	writeFile(t, filepath.Join(bin, "claude"), "#!/bin/sh\n: > "+ran+"\n")
	if err := os.Chmod(filepath.Join(bin, "claude"), 0o700); err != nil { //nolint:gosec // a fake claude
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	c := chromeOnMusic()
	stubCompositor(t, c)
	lockScreen(t, hyprlock)
	err := a.browse(t.Context(), "Open the portal.", bin, "", time.Minute, "", nil, nil)
	if !errors.Is(err, errScreenLocked) || !strings.HasPrefix(err.Error(), "screen locked: hyprlock runs") ||
		!strings.Contains(err.Error(), "Park until the screen unlocks") || !strings.Contains(err.Error(), `--probe "! pgrep -x hyprlock"`) {
		t.Fatalf("browse under a locked screen = %v", err)
	}
	if _, serr := os.Stat(ran); serr == nil || out.Len() != 0 || len(c.calls) != 0 {
		t.Errorf("browse under a locked screen ran a turn (%v), printed %q or ran %q", serr, out.String(), c.calls)
	}
	st, rerr := a.store.Read()
	if rerr != nil || len(st.Starts) != 0 {
		t.Errorf("browse under a locked screen recorded starts %+v (%v)", st.Starts, rerr)
	}
	lockScreen(t, "")
	if err := screenLockedErr(); err != nil {
		t.Errorf("an unlocked screen was refused: %v", err)
	}
}

// A window browse could not draw is named where it is in the draw error;
// one drawn on the output and still not drawn names what the turn tried.
func TestDrawErrNamesWhereTheWindowIs(t *testing.T) {
	failed := "the shared Chrome window is on workspace special:music, not shown on any monitor; drawing it on the off-screen output BEEKEEPER-BROWSE failed: no socket"
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
	if l := (placement{Opened: 1, Back: "2"}).line(); l != "windows opened: 1 window(s) Chrome opened during the turn were drawn on the off-screen output BEEKEEPER-BROWSE, "+
		"never on the person's screen, and were still open after it: they went to the shared window's workspace 2" {
		t.Errorf("line of a pop-up left open = %q", l)
	}
	where := "workspace 2, under the special workspace special:magic shown over it on eDP-1"
	f = browseFindings{NotDrawn: 2, Window: placement{Where: where, To: hypr.Output, Restored: true}}
	if err := f.drawErr(); err == nil || !strings.Contains(err.Error(), "the turn drew it on the off-screen output BEEKEEPER-BROWSE") || !strings.Contains(err.Error(), "on "+where) {
		t.Errorf("drawErr = %v, want it to name where the window was and what the turn tried", err)
	}
}
