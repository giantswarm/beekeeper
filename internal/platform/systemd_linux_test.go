//go:build linux && !nosystemd

package platform

import (
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

func TestOpenerRunning(t *testing.T) {
	at := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	table := func(args ...[]string) *proc.Table {
		tb := &proc.Table{ByPID: map[int]*proc.Process{}}
		for i, a := range args {
			tb.ByPID[i+1] = &proc.Process{PID: i + 1, Args: a, Start: at}
		}
		return tb
	}
	renderer := []string{"/usr/lib/claude-desktop/claude-desktop --type=renderer --lang=en"}
	for name, tc := range map[string]struct {
		t    *proc.Table
		want time.Time
	}{
		"the argument vector":             {table([]string{"/usr/lib/claude-desktop/claude-desktop", "--ozone-platform=wayland"}), at},
		"a command line Electron rewrote": {table(renderer, []string{"/usr/lib/claude-desktop/claude-desktop --ozone-platform=wayland --password-store=gnome-libsecret"}), at},
		"only its helpers":                {table(renderer, []string{"/usr/lib/claude-desktop/chrome_crashpad_handler"}), time.Time{}},
	} {
		if got := (systemdOpener{app: desktopApp}.Running(tc.t)); !got.Equal(tc.want) {
			t.Errorf("%s: Running = %v, want %v", name, got, tc.want)
		}
	}
}
