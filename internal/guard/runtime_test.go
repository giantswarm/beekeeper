package guard

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
)

const (
	runtimeAllPackages  = "./..."
	runtimeSleepCommand = "sleep"
)

func TestRunTimeout(t *testing.T) {
	for _, tt := range []struct {
		o    Options
		argv []string
		want time.Duration
	}{
		{Options{}, []string{"go", testCommand, runtimeAllPackages}, 30 * time.Minute},
		{Options{}, []string{runtimeLintCommand, executionRunCommand}, 5 * time.Minute},
		{Options{}, []string{"sh", "-c", "cd /tmp && golangci-lint run"}, 5 * time.Minute},
		{Options{Class: ClassLab}, []string{"agentlab", "prove"}, 0},
		{Options{Class: ClassBenchmark}, []string{"bench"}, 0},
		{Options{Timeout: time.Hour}, []string{runtimeLintCommand, executionRunCommand}, time.Hour},
	} {
		if got := RunTimeout(tt.o, tt.argv); got != tt.want {
			t.Errorf("%v: got %s want %s", tt.argv, got, tt.want)
		}
	}
	argv := []string{runtimeLintCommand, executionRunCommand, runtimeAllPackages}
	if got := lintTimeout(argv, 5*time.Minute); !reflect.DeepEqual(got, []string{runtimeLintCommand, executionRunCommand, "--timeout=5m0s", runtimeAllPackages}) {
		t.Fatal(got)
	}
	argv = []string{runtimeLintCommand, executionRunCommand, "--timeout", "10m"}
	if !reflect.DeepEqual(lintTimeout(argv, time.Minute), argv) {
		t.Fatal("overwrote explicit timeout")
	}
}

func TestLintTimeoutSlotlessClasses(t *testing.T) {
	for _, class := range []string{ClassLab, ClassBenchmark} {
		t.Run(class, func(t *testing.T) {
			argv := []string{runtimeLintCommand, executionRunCommand, runtimeAllPackages}
			for _, timeout := range []time.Duration{0, time.Hour} {
				deadline := RunTimeout(Options{Class: class, Timeout: timeout}, argv)
				want := argv
				if timeout > 0 {
					want = []string{runtimeLintCommand, executionRunCommand, "--timeout=1h0m0s", runtimeAllPackages}
				}
				if got := lintTimeout(argv, deadline); !reflect.DeepEqual(got, want) {
					t.Errorf("override %s: got %v, want %v", timeout, got, want)
				}
			}
		})
	}
}

type deadlineCapper struct {
	fakeCapper
	command *proc.Cmd
	stopped bool
}

func (f *deadlineCapper) Command(_ string, _ platform.Cap, argv []string) (*proc.Cmd, error) {
	f.command = proc.CommandContext(context.Background(), argv[0], argv[1:]...) //nolint:gosec // argv contains only this test suite's owned fixture commands
	return f.command, nil
}
func (f *deadlineCapper) Reap(string) ([]int, error) {
	f.stopped = true
	return nil, f.command.Process.Kill()
}

func TestScopeExecutionDeadline(t *testing.T) {
	f := &deadlineCapper{}
	var log strings.Builder
	rc, drain := scope(f, testCommand, Options{Timeout: 20 * time.Millisecond, Stderr: io.Discard}, 1, []string{runtimeSleepCommand, "30"}, nil, func(s string, a ...any) { fmt.Fprintf(&log, s, a...) })
	drain()
	if rc != ExitTimeout || !f.stopped || !strings.Contains(log.String(), "timed out") {
		t.Fatalf("exit %d stopped %t log %s", rc, f.stopped, log.String())
	}
}

func TestRunTimeoutReleasesSlot(t *testing.T) {
	f := &deadlineCapper{}
	savedC, savedM := plat.Capper, plat.Machine
	plat.Capper, plat.Machine = f, noKills{Machine: savedM}
	t.Cleanup(func() { plat.Capper, plat.Machine = savedC, savedM })
	var records []string
	o := Options{Max: "1M", Swap: "0", SlotDir: t.TempDir(), Slots: 1, Stderr: io.Discard, Timeout: 20 * time.Millisecond,
		Record: func(verb, detail string) {
			if verb == VerbEnd {
				records = append(records, detail)
			}
		}}
	if rc := Run(o, []string{runtimeSleepCommand, "30"}); rc != ExitTimeout {
		t.Fatalf("exit %d", rc)
	}
	if _, err := os.Stat(filepath.Join(o.SlotDir, "1.holder")); !os.IsNotExist(err) {
		t.Fatalf("holder retained: %v", err)
	}
	o.Timeout = time.Second
	if rc := Run(o, trueCmd); rc != 0 {
		t.Fatalf("next run: %d", rc)
	}
	if len(records) != 2 || !strings.Contains(records[0], "exit 124") {
		t.Fatalf("records %v", records)
	}
}
