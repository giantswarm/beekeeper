//go:build linux && !nosystemd

package platform

import (
	"math"
	"strings"
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
		if got := (systemdOpener{app: "claude-desktop"}.Running(tc.t)); !got.Equal(tc.want) {
			t.Errorf("%s: Running = %v, want %v", name, got, tc.want)
		}
	}
}

func TestSizeBytes(t *testing.T) {
	for in, want := range map[string]uint64{"0": 0, "512": 512, "4K": 4 << 10, "12G": 12 << 30, "1536m": 1536 << 20, "1T": 1 << 40, "infinity": math.MaxUint64} {
		if got, err := sizeBytes(in); err != nil || got != want {
			t.Errorf("sizeBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "G", "-1G", "1.5G", "12GB", "99999999999T"} {
		if _, err := sizeBytes(in); err == nil {
			t.Errorf("sizeBytes(%q): want an error", in)
		}
	}
}

// A unit's informative ends never fail it: a stop as asked with
// TermIsSuccess, and the stop-post's own end, a kill included.
func TestRunArgsExpectedEndsSucceed(t *testing.T) {
	got := strings.Join(runArgs(Unit{Name: "beekeeper-wake-x", Argv: []string{"claude"}, TermIsSuccess: true,
		StopPost: []string{"/bin/beekeeper", "agents", "reopen", "local_x"}, StopTimeout: time.Minute}), " ")
	for _, want := range []string{"SuccessExitStatus=143 SIGTERM", "ExecStopPost=-/bin/beekeeper agents reopen local_x", "-- claude"} {
		if !strings.Contains(got, want) {
			t.Errorf("systemd-run %s: no %q", got, want)
		}
	}
}
