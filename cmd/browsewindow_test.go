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
	// special is the special workspace the person's monitor shows over
	// workspace 2; redirect says the turn's window rule is on.
	special  hypr.Workspace
	redirect bool
}

func (c *compositor) ctl(_ context.Context, args ...string) ([]byte, error) {
	switch args[len(args)-1] {
	case "clients":
		return json.Marshal([]hypr.Window{c.window})
	case "monitors":
		ms := []hypr.Monitor{{Name: "eDP-1", Width: 1920, Height: 1200, Focused: true, ActiveWorkspace: hypr.Workspace{ID: 2, Name: "2"}, SpecialWorkspace: c.special}}
		if c.output {
			ms = append(ms, hypr.Monitor{ID: 1, Name: hypr.Output, ActiveWorkspace: hypr.Workspace{ID: hypr.SurfaceID, Name: hypr.Surface}})
		}
		return json.Marshal(ms)
	case "general:layout":
		return []byte(`{"str":"dwindle"}`), nil
	}
	call := strings.Join(args, " ")
	c.calls = append(c.calls, call)
	switch {
	case args[0] == "eval" && strings.Contains(call, "set_enabled(true)"):
		c.redirect = true
	case args[0] == "eval" && strings.Contains(call, "set_enabled(false)"):
		c.redirect = false
	case call == "output create headless "+hypr.Output:
		c.output = true
	case call == "output remove "+hypr.Output:
		c.output = false
	case strings.Contains(call, `workspace = "name:`+hypr.Surface+`"`):
		c.window.Workspace = hypr.Workspace{ID: hypr.SurfaceID, Name: hypr.Surface}
	case strings.Contains(call, `workspace = "`+musicWS.Name+`"`):
		c.window.Workspace = musicWS
	}
	return []byte("ok"), nil
}

// chromeClass is Chrome's browser window class.
const chromeClass = "google-chrome"

// musicWS is the special workspace Chrome starts on, not shown.
var musicWS = hypr.Workspace{ID: -95, Name: "special:music"}

// chromeOnMusic is a compositor with Chrome on special:music, not shown.
func chromeOnMusic() *compositor {
	return &compositor{window: hypr.Window{Address: "0xc", Class: chromeClass, Workspace: musicWS}}
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

// typedAt stubs the person's input: the watch's first answer is start, its
// later ones *last (start: no input yet); unreadable fails the watch.
func typedAt(t *testing.T, start time.Time, last *time.Time, unreadable bool) {
	t.Helper()
	saved, sample, poll := desktopInput, awaySample, presencePoll
	t.Cleanup(func() { desktopInput, awaySample, presencePoll = saved, sample, poll })
	awaySample, presencePoll = time.Millisecond, time.Millisecond
	*last = start
	desktopInput = func(context.Context) (func() time.Time, error) {
		if unreadable {
			return nil, errors.New("permission denied")
		}
		first := true
		return func() time.Time {
			if first {
				first = false
				return start
			}
			return *last
		}, nil
	}
}

// A browse refuses a turn while the person works at Chrome: a Chrome window
// in their view and their input in the sample, or unreadable input; it
// names the probe. With Chrome out of view, or the person not typing, it
// starts the turn.
func TestAtChrome(t *testing.T) {
	_, _ = stubApp(t)
	start := time.Now()
	focused := hypr.Window{Address: "0xc", Class: chromeClass, Workspace: hypr.Workspace{ID: 2, Name: "2"}}
	hidden := hypr.Window{Address: "0xc", Class: chromeClass, Workspace: musicWS, FocusHistoryID: 4}
	for name, tc := range map[string]struct {
		window     hypr.Window
		typed      bool
		unreadable bool
		quiet      time.Duration
		want       string
	}{
		"focused and typing":         {window: focused, typed: true, quiet: time.Minute, want: "a Chrome window has the focus and their keyboard or pointer was in use"},
		"focused and unreadable":     {window: focused, unreadable: true, quiet: time.Minute, want: "cannot be read, which counts as their input"},
		"shown and typing":           {window: with(focused, 4), typed: true, quiet: time.Minute, want: "a Chrome window is shown on workspace 2"},
		"focused just before typing": {window: with(hidden, 1), typed: true, quiet: time.Minute, want: "had the focus before the current window"},
		"focused and quiet":          {window: focused, quiet: time.Minute},
		"hidden and typing":          {window: hidden, typed: true, quiet: time.Minute},
		"input not watched":          {window: focused, typed: true, quiet: -time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			var last time.Time
			typedAt(t, start, &last, tc.unreadable)
			if tc.typed {
				last = start.Add(time.Millisecond)
			}
			stubCompositor(t, &compositor{window: tc.window})
			_, err := atChrome(t.Context(), tc.quiet)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("atChrome = %v, want a turn", err)
			case tc.want != "" && (!errors.Is(err, errPersonAtChrome) || !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), `--probe "beekeeper browse away"`)):
				t.Errorf("atChrome = %v, want person at Chrome: %s, and the probe", err, tc.want)
			}
		})
	}
}

// with is w with the focus history id id.
func with(w hypr.Window, id int) hypr.Window { w.FocusHistoryID = id; return w }

// Over the turn the watch turns the redirect off while the person types,
// on again once they are quiet, and yields the window once they turn to its
// workspace: the turn is cancelled as person at Chrome and the window is
// back at once.
func TestFollowYieldsTheWindow(t *testing.T) {
	_, _ = stubApp(t)
	lockScreen(t, "")
	start := time.Now()
	var last time.Time
	typedAt(t, start, &last, false)
	c := chromeOnMusic()
	c.window.FocusHistoryID = 4
	stubCompositor(t, c)
	p := showChrome(t.Context(), filepath.Join(t.TempDir(), "window.json"), time.Hour)
	if p.To != hypr.Output || !c.redirect {
		t.Fatalf("placement = %+v, redirect %v", p, c.redirect)
	}
	pres := watchPresence(t.Context(), time.Minute)
	var cause error
	cancel := func(err error) { cause = err }

	if p.follow(t.Context(), pres, time.Hour, cancel) || !c.redirect {
		t.Fatalf("quiet: yielded, or redirect %v", c.redirect)
	}
	last = time.Now()
	if p.follow(t.Context(), pres, time.Hour, cancel) || c.redirect || c.window.Workspace.Name != hypr.Surface {
		t.Fatalf("typing elsewhere: redirect %v, Chrome on %+v", c.redirect, c.window.Workspace)
	}
	c.special = musicWS
	if !p.follow(t.Context(), pres, time.Hour, cancel) {
		t.Fatal("the person turned to the window's workspace and the turn did not yield")
	}
	if !errors.Is(cause, errPersonAtChrome) || !strings.Contains(cause.Error(), "eDP-1 shows workspace special:music") {
		t.Errorf("cancel cause = %v", cause)
	}
	if c.window.Workspace != musicWS || c.output || c.redirect || !p.Restored || p.Watch != "" {
		t.Errorf("after the yield: Chrome on %+v, output %v, redirect %v, placement %+v", c.window.Workspace, c.output, c.redirect, p)
	}
	if l := p.line(); !strings.Contains(l, "window yielded: during the turn eDP-1 shows workspace special:music") {
		t.Errorf("line = %q", l)
	}
	n := len(c.calls)
	if err := p.restore(t.Context()); err != nil || len(c.calls) != n {
		t.Errorf("a restore after the yield = %v, ran %q", err, c.calls[n:])
	}
}

// The watch runs until stop, and stop waits for it; without a placement it
// watches nothing.
func TestWatchStops(t *testing.T) {
	_, _ = stubApp(t)
	lockScreen(t, "")
	start := time.Now()
	var last time.Time
	typedAt(t, start, &last, false)
	c := chromeOnMusic()
	stubCompositor(t, c)
	p := showChrome(t.Context(), filepath.Join(t.TempDir(), "window.json"), time.Hour)
	pres := watchPresence(t.Context(), time.Minute)
	stop := p.watch(t.Context(), pres, time.Hour, func(error) {})
	stop()
	stop()
	if p.Yielded != "" || c.window.Workspace.Name != hypr.Surface {
		t.Errorf("a quiet watch yielded: %+v", p)
	}
	(&placement{}).watch(t.Context(), pres, time.Hour, func(error) {})()
}
