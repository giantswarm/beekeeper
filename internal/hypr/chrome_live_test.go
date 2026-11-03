//go:build hyprlive

package hypr

// The live proof of a browse turn's placement on the running compositor,
// with a scratch-profile Chrome of its own class and never the person's
// windows or Chrome profile: a pop-up the turn's page opens lands on the
// off-screen output, and the person's monitors, workspaces and focus are
// the same before, during and after. Run it as
//
//	BEEKEEPER_LIVE_DIR=<scratch dir> go test -tags hyprlive -run Live -v ./internal/hypr
//
// It writes the snapshots and the compositor's events to the scratch dir.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// liveClass is the scratch Chrome's Wayland class; no rule of the person's
// matches it.
const liveClass = "beekeeper-proof"

// hidden is the workspace the scratch Chrome starts on: named, shown on no
// monitor.
const hidden = "bk-proof"

// view is what the person sees: their monitors (place, size, shown and
// special workspaces, focus), the focused window and the workspaces there
// are, the proof's own left out.
type view struct {
	Monitors   []Monitor `json:"monitors"`
	Focused    string    `json:"focused"`
	Workspaces []string  `json:"workspaces"`
}

func snapshot(t *testing.T, ctx context.Context) view {
	t.Helper()
	l, err := read(ctx, Hyprctl)
	if err != nil {
		t.Fatal(err)
	}
	var active struct {
		Address string `json:"address"`
	}
	var wss []Workspace
	if err := query(ctx, Hyprctl, &active, "activewindow"); err != nil {
		t.Fatal(err)
	}
	if err := query(ctx, Hyprctl, &wss, "workspaces"); err != nil {
		t.Fatal(err)
	}
	v := view{Monitors: l.view(), Focused: active.Address}
	for _, ws := range wss {
		if ws.Name != hidden && ws.Name != hiddenNumber && ws.Name != Surface {
			v.Workspaces = append(v.Workspaces, ws.Name)
		}
	}
	slices.Sort(v.Workspaces)
	return v
}

func (v view) same(o view) bool {
	return slices.Equal(v.Monitors, o.Monitors) && v.Focused == o.Focused && slices.Equal(v.Workspaces, o.Workspaces)
}

// events records the compositor's event stream until stop.
func events(t *testing.T) (stop func() []string) {
	t.Helper()
	sock := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "hypr", os.Getenv("HYPRLAND_INSTANCE_SIGNATURE"), ".socket2.sock")
	c, err := net.Dial("unix", sock) //nolint:gosec // the compositor's own event socket
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu  sync.Mutex
		got []string
		wg  sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(c)
		for sc.Scan() {
			mu.Lock()
			got = append(got, time.Now().Format("15:04:05.000")+" "+sc.Text())
			mu.Unlock()
		}
	}()
	return func() []string {
		_ = c.Close()
		wg.Wait()
		return got
	}
}

// proofWindows are the scratch Chrome's windows.
func proofWindows(t *testing.T, ctx context.Context) []Window {
	t.Helper()
	l, err := read(ctx, Hyprctl)
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(l.windows, func(w Window) bool { return w.Class != liveClass })
}

// waitWindows waits for n scratch Chrome windows, 20s at most.
func waitWindows(t *testing.T, ctx context.Context, n int) []Window {
	t.Helper()
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if ws := proofWindows(t, ctx); len(ws) >= n {
			return ws
		}
	}
	t.Fatalf("no %d %s windows within 20s: %+v", n, liveClass, proofWindows(t, ctx))
	return nil
}

func TestLivePopUpLandsOnTheOutput(t *testing.T) {
	dir := os.Getenv("BEEKEEPER_LIVE_DIR")
	if !Running() || dir == "" {
		t.Skip("needs Hyprland and BEEKEEPER_LIVE_DIR")
	}
	ctx := context.Background()
	classes := browserClasses
	browserClasses = []string{liveClass}
	t.Cleanup(func() { browserClasses = classes })
	profile := filepath.Join(dir, "profile")
	page := filepath.Join(dir, "popup.html")
	if err := os.WriteFile(page, []byte(`<title>beekeeper proof</title><script>`+ //nolint:gosec // the proof's scratch dir
		`window.open("about:blank#beekeeper-proof-pop-up", "bkpopup", "popup,width=480,height=360")</script>`), 0o600); err != nil {
		t.Fatal(err)
	}
	chrome := []string{"google-chrome-stable", "--user-data-dir=" + profile, "--class=" + liveClass, "--no-first-run",
		"--no-default-browser-check", "--password-store=basic", "--disable-popup-blocking"}
	record := func(name string, v any) {
		b, _ := json.MarshalIndent(v, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o600); err != nil { //nolint:gosec // the proof's scratch dir
			t.Fatal(err)
		}
	}

	stop := events(t)
	before := snapshot(t, ctx)
	record("1-before.json", before)

	// The scratch Chrome starts on a workspace no monitor shows, silently.
	launch := fmt.Sprintf("hl.exec_cmd(%q, { workspace = %q, no_initial_focus = true })", strings.Join(append(chrome, "about:blank"), " "), "name:"+hidden+" silent")
	if err := run(ctx, Hyprctl, "eval", launch); err != nil {
		t.Fatal(err)
	}
	main := waitWindows(t, ctx, 1)[0]
	t.Cleanup(func() {
		if p, err := os.FindProcess(main.Pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	})
	if main.Workspace.Name != hidden {
		t.Fatalf("the scratch Chrome started on %+v", main.Workspace)
	}

	// The turn: the window goes to the output, the redirect is on.
	s, err := Show(ctx, Hyprctl, 2*time.Minute)
	if err != nil || s == nil || !s.Moved {
		t.Fatalf("Show = %+v, %v", s, err)
	}
	record("2-shown.json", s)
	during := snapshot(t, ctx)
	record("3-during-shown.json", during)

	// The turn's page opens a pop-up: a second scratch Chrome command hands
	// the page to the running one, which opens it in a tab; the page opens
	// the pop-up.
	if out, err := exec.CommandContext(ctx, chrome[0], append(chrome[1:], "file://"+page)...).CombinedOutput(); err != nil { //nolint:gosec // the proof's own Chrome
		t.Fatalf("%s: %v", out, err)
	}
	wins := waitWindows(t, ctx, 2)
	record("4-windows-during.json", wins)
	popUp := snapshot(t, ctx)
	record("5-during-pop-up.json", popUp)
	for _, w := range wins {
		if w.Workspace.Name != Surface {
			t.Errorf("window %s (%s) mapped on %+v, not %s", w.Address, w.Class, w.Workspace, Surface)
		}
	}

	if err := s.Restore(ctx, Hyprctl); err != nil {
		t.Fatalf("Restore = %v", err)
	}
	record("6-restored.json", s)
	after := snapshot(t, ctx)
	record("7-after.json", after)
	record("8-windows-after.json", proofWindows(t, ctx))
	l, err := read(ctx, Hyprctl)
	if err != nil {
		t.Fatal(err)
	}
	if l.output() != nil {
		t.Errorf("the output %s outlived the turn", Output)
	}
	ev := stop()
	if err := os.WriteFile(filepath.Join(dir, "9-events.txt"), []byte(strings.Join(ev, "\n")+"\n"), 0o600); err != nil { //nolint:gosec // the proof's scratch dir
		t.Fatal(err)
	}
	for name, v := range map[string]view{"during the turn": during, "with the pop-up open": popUp, "after the turn": after} {
		if !v.same(before) {
			t.Errorf("the person's view %s = %+v, before %+v", name, v, before)
		}
	}
	var ours []string
	for _, w := range wins {
		ours = append(ours, "activewindowv2>>"+strings.TrimPrefix(w.Address, "0x"))
	}
	for _, e := range ev {
		if slices.ContainsFunc([]string{"activewindowv2>>", "focusedmon>>", "workspacev2>>", "fullscreen>>"}, func(k string) bool { return strings.Contains(e, " "+k) }) {
			t.Logf("focus or workspace event: %s", e)
		}
		if slices.ContainsFunc(ours, func(a string) bool { return strings.Contains(e, " "+a) }) {
			t.Errorf("a proof window took the focus: %s", e)
		}
	}
	t.Logf("scratch Chrome pid %d, %d windows, %d events", main.Pid, len(wins), len(ev))
}
