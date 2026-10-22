//go:build hyprlive

package hypr

// The live proof that a re-created output shows its own workspace whatever
// named workspaces were made since: a turn, then a named workspace made
// between turns (it takes the first named workspace number, which the named
// beekeeper-browse of an earlier beekeeper had), then a second turn. Run it
// as
//
//	BEEKEEPER_LIVE_DIR=<scratch dir> go test -tags hyprlive -run LiveSurface -v ./internal/hypr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLiveSurfaceAfterNamedWorkspace(t *testing.T) {
	dir := os.Getenv("BEEKEEPER_LIVE_DIR")
	if !Running() || dir == "" {
		t.Skip("needs Hyprland and BEEKEEPER_LIVE_DIR")
	}
	ctx := context.Background()
	classes := browserClasses
	browserClasses = []string{liveClass}
	t.Cleanup(func() { browserClasses = classes })
	chrome := []string{"google-chrome-stable", "--user-data-dir=" + filepath.Join(dir, "profile"), "--class=" + liveClass,
		"--no-first-run", "--no-default-browser-check", "--password-store=basic", "about:blank"}
	launch := fmt.Sprintf("hl.exec_cmd(%q, { workspace = %q, no_initial_focus = true })", strings.Join(chrome, " "), hiddenNumber+" silent")
	if err := run(ctx, Hyprctl, "eval", launch); err != nil {
		t.Fatal(err)
	}
	main := waitWindows(t, ctx, 1)[0]
	t.Cleanup(func() {
		if p, err := os.FindProcess(main.Pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	})
	before := snapshot(t, ctx)

	turn := func(name string) Workspace {
		t.Helper()
		s, err := Show(ctx, Hyprctl, time.Minute)
		if err != nil || s == nil || !s.Moved {
			t.Fatalf("%s: Show = %+v, %v", name, s, err)
		}
		l, err := read(ctx, Hyprctl)
		if err != nil {
			t.Fatal(err)
		}
		shown := l.output().ActiveWorkspace
		if err := s.Restore(ctx, Hyprctl); err != nil {
			t.Fatalf("%s: Restore = %v", name, err)
		}
		t.Logf("%s: the output showed %+v; the window went back to %s", name, shown, s.Before.Workspace.Name)
		return shown
	}
	if ws := turn("first turn"); ws != surface {
		t.Errorf("first turn: the output showed %+v, not %+v", ws, surface)
	}

	// Between turns a named workspace is made, the scratch window on it.
	if err := moveTo(main.Address, Workspace{Name: hidden}).run(ctx, Hyprctl); err != nil {
		t.Fatal(err)
	}
	var named Window
	for _, w := range proofWindows(t, ctx) {
		if w.Address == main.Address {
			named = w
		}
	}
	t.Logf("between turns: the named workspace %+v", named.Workspace)

	if ws := turn("second turn"); ws != surface {
		t.Errorf("second turn: the output showed %+v, not %+v", ws, surface)
	}
	for _, w := range proofWindows(t, ctx) {
		if w.Address == main.Address && w.Workspace != named.Workspace {
			t.Errorf("the scratch window went back to %+v, not %+v", w.Workspace, named.Workspace)
		}
	}
	if err := moveTo(main.Address, Workspace{ID: 77, Name: hiddenNumber}).run(ctx, Hyprctl); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, ctx); !after.same(before) {
		t.Errorf("the person's view after = %+v, before %+v", after, before)
	}
}
