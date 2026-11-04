package guard

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/platform"
)

func TestRunTimeout(t *testing.T) {
	for _, tt := range []struct {
		o    Options
		argv []string
		want time.Duration
	}{
		{Options{}, []string{"go", "test", "./..."}, 30 * time.Minute},
		{Options{}, []string{"golangci-lint", "run"}, 5 * time.Minute},
		{Options{}, []string{"/usr/local/bin/golangci-lint", "run"}, 5 * time.Minute},
		{Options{}, []string{"./bin/golangci-lint", "run"}, 5 * time.Minute},
		{Options{}, []string{"sh", "-c", "cd /tmp && /usr/local/bin/golangci-lint run"}, 5 * time.Minute},
		{Options{}, []string{"sh", "-c", "./bin/golangci-lint run"}, 5 * time.Minute},
		{Options{}, []string{"echo", "/usr/local/bin/golangci-lint", "run"}, 30 * time.Minute},
		{Options{}, []string{"/usr/local/bin/golangci-lint", "--help"}, 30 * time.Minute},
		{Options{Class: ClassLab}, []string{"/usr/local/bin/golangci-lint", "run"}, 0},
		{Options{Timeout: time.Hour}, []string{"/usr/local/bin/golangci-lint", "run"}, time.Hour},
		{Options{}, []string{"sh", "-c", "cd /tmp && golangci-lint run"}, 5 * time.Minute},
		{Options{Class: ClassLab}, []string{"agentlab", "prove"}, 0},
		{Options{Class: ClassBenchmark}, []string{"bench"}, 0},
		{Options{Timeout: time.Hour}, []string{"golangci-lint", "run"}, time.Hour},
	} {
		if got := RunTimeout(tt.o, tt.argv); got != tt.want {
			t.Errorf("%v: got %s want %s", tt.argv, got, tt.want)
		}
	}
	argv := []string{"golangci-lint", "run", "./..."}
	if got := lintTimeout(argv, 5*time.Minute); !reflect.DeepEqual(got, []string{"golangci-lint", "run", "--timeout=5m0s", "./..."}) {
		t.Fatal(got)
	}
	argv = []string{"golangci-lint", "run", "--timeout", "10m"}
	if !reflect.DeepEqual(lintTimeout(argv, time.Minute), argv) {
		t.Fatal("overwrote explicit timeout")
	}
}

func TestLintTimeoutExecutablePaths(t *testing.T) {
	for _, executable := range []string{"/usr/local/bin/golangci-lint", "./bin/golangci-lint"} {
		argv := []string{executable, "run", "./..."}
		got := lintTimeout(argv, RunTimeout(Options{}, argv))
		want := []string{executable, "run", "--timeout=5m0s", "./..."}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

func TestLintTimeoutSlotlessClasses(t *testing.T) {
	for _, class := range []string{ClassLab, ClassBenchmark} {
		t.Run(class, func(t *testing.T) {
			argv := []string{"golangci-lint", "run", "./..."}
			for _, timeout := range []time.Duration{0, time.Hour} {
				deadline := RunTimeout(Options{Class: class, Timeout: timeout}, argv)
				want := argv
				if timeout > 0 {
					want = []string{"golangci-lint", "run", "--timeout=1h0m0s", "./..."}
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
	command *exec.Cmd
	stopped bool
}

func (f *deadlineCapper) Command(_ string, _ platform.Cap, argv []string) (*exec.Cmd, error) {
	f.command = exec.Command(argv[0], argv[1:]...)
	return f.command, nil
}
func (f *deadlineCapper) Reap(string) ([]int, error) {
	f.stopped = true
	return nil, f.command.Process.Kill()
}

func TestScopeExecutionDeadline(t *testing.T) {
	f := &deadlineCapper{}
	var log strings.Builder
	rc, drain := scope(f, "test", Options{Timeout: 20 * time.Millisecond, Stderr: io.Discard}, 1, []string{"sleep", "30"}, nil, func(s string, a ...any) { fmt.Fprintf(&log, s, a...) })
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
	if rc := Run(o, []string{"sleep", "30"}); rc != ExitTimeout {
		t.Fatalf("exit %d", rc)
	}
	if _, err := os.Stat(filepath.Join(o.SlotDir, "1.holder")); !os.IsNotExist(err) {
		t.Fatalf("holder retained: %v", err)
	}
	o.Timeout = time.Second
	if rc := Run(o, []string{"true"}); rc != 0 {
		t.Fatalf("next run: %d", rc)
	}
	if len(records) != 2 || !strings.Contains(records[0], "exit 124") {
		t.Fatalf("records %v", records)
	}
}
