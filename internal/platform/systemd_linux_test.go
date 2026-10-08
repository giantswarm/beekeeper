//go:build linux && !nosystemd

package platform

import (
	"math"
	"reflect"
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

// The properties a slot's cap sets: memcap.slice's CPU budget first, with
// a CPUWeight, then the slot slice's memory; a scope's systemd-run
// arguments carry the slot's slice, the memory cap and RunNice.
func TestCapProperties(t *testing.T) {
	const slot, noSwap = "memcap-slot1_0a1b2c3d.slice", "MemorySwapMax=0"
	c := Cap{Max: "12G", Swap: "0", Slice: slot, CPUQuota: "1200%", CPUWeight: 50}
	want := [][]string{
		{memcapSlice, "CPUQuota=1200%", "CPUWeight=50"},
		{slot, "MemoryMax=12G", noSwap},
	}
	if got := sliceProperties(c); !reflect.DeepEqual(got, want) {
		t.Errorf("sliceProperties = %q, want %q", got, want)
	}
	// An unknown core count leaves the quota empty, which lifts it.
	c.CPUQuota = ""
	if got := sliceProperties(c); !reflect.DeepEqual(got[0], []string{memcapSlice, "CPUQuota=", "CPUWeight=50"}) {
		t.Errorf("without a quota: %q", got[0])
	}
	// Without a weight the slice's CPU stays as it is: the memory only.
	c.CPUWeight = 0
	if got := sliceProperties(c); !reflect.DeepEqual(got, want[1:]) {
		t.Errorf("without a weight: %q", got)
	}
	// No slot: memcap.slice itself takes the memory cap.
	if got := sliceProperties(Cap{Max: "512M", Swap: "0"}); !reflect.DeepEqual(got, [][]string{{memcapSlice, "MemoryMax=512M", noSwap}}) {
		t.Errorf("without a slot: %q", got)
	}

	args := scopeArgs("memcap-42-000001", c, []string{"zsh", "-c", "go build ./..."})
	wantArgs := []string{"--user", "--scope", "--quiet", "--expand-environment=no", "--unit=memcap-42-000001",
		"--slice=" + slot, "--nice=10", "-p", "MemoryMax=12G", "-p", noSwap, "-p", "OOMPolicy=continue",
		"--", "zsh", "-c", "go build ./..."}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("scopeArgs = %q, want %q", args, wantArgs)
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
	got := strings.Join(runArgs(Unit{Name: "beekeeper-wake-x", Argv: []string{"claude"}, TermIsSuccess: true, StopPost: detachStopPost, StopTimeout: time.Minute}), " ")
	for _, want := range []string{"SuccessExitStatus=143 SIGTERM", "ExecStopPost=-/bin/beekeeper agents reopen --detach local_x", "-- claude"} {
		if !strings.Contains(got, want) {
			t.Errorf("systemd-run %s: no %q", got, want)
		}
	}
}

// detachStopPost is a turn unit's stop-post, which starts the reopen's unit.
var detachStopPost = []string{"/bin/beekeeper", "agents", "reopen", "--detach", "local_x"}

// A turn's unit is bounded in its stop (TimeoutStopSec, the stop-post
// within it), a reopen's in its runtime (RuntimeMaxSec); a unit without
// either gets the manager's defaults.
func TestRunArgsStopAndRuntimeBounds(t *testing.T) {
	for name, c := range map[string]struct {
		unit      Unit
		want, not []string
	}{
		"a turn": {Unit{Name: "beekeeper-wake-x", Argv: []string{"claude"}, StopPost: detachStopPost, StopTimeout: time.Minute},
			[]string{"-p TimeoutStopSec=60 "}, []string{"RuntimeMaxSec"}},
		"a reopen": {Unit{Name: "beekeeper-reopen-x", Argv: []string{"beekeeper"}, MaxRuntime: 35 * time.Minute},
			[]string{"-p RuntimeMaxSec=2100 "}, []string{"ExecStopPost", "TimeoutStopSec"}},
		"neither": {Unit{Name: "beekeeper-merge-x", Argv: []string{"devctl"}}, nil, []string{"ExecStopPost", "TimeoutStopSec", "RuntimeMaxSec"}},
	} {
		got := strings.Join(runArgs(c.unit), " ")
		for _, want := range c.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s: systemd-run %s: no %q", name, got, want)
			}
		}
		for _, not := range c.not {
			if strings.Contains(got, not) {
				t.Errorf("%s: systemd-run %s: has %q", name, got, not)
			}
		}
	}
}
