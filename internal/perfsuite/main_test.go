//go:build perf && linux

package perfsuite

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The test executable doubles as the fixture's fake GitHub CLI, SSH client,
// harnesses and Git wrapper; beekeeper itself is the separately built binary
// under test.
func TestMain(m *testing.M) {
	if code, served := serveFake(); served {
		os.Exit(code)
	}
	if code, served := serveMeasure(); served {
		os.Exit(code)
	}
	code := m.Run()
	if err := rep.write(); err != nil {
		fmt.Fprintln(os.Stderr, "performance report:", err)
		if code == 0 {
			code = 1
		}
	}
	cleanupBinary()
	os.Exit(code)
}

// requirePerf skips without BEEKEEPER_PERF=1 and refuses a run that could
// load the machine it runs on: outside CI the process must sit in a cgroup
// whose CPU and memory are both limited (make test-performance-local).
func requirePerf(t *testing.T) {
	t.Helper()
	if os.Getenv("BEEKEEPER_PERF") != "1" {
		t.Skip("performance suite: set BEEKEEPER_PERF=1 (CI, or make test-performance-local)")
	}
	if os.Getenv("CI") == "true" {
		return
	}
	cpu, mem := cgroupLimited()
	if !cpu || !mem {
		t.Fatalf("refusing a local performance run outside a cgroup limit (cpu limited: %t, memory limited: %t): run make test-performance-local", cpu, mem)
	}
}

// cgroupLimited reports whether this process's cgroup v2, or one of its
// ancestors, caps CPU (cpu.max) and memory (memory.max).
func cgroupLimited() (cpu, mem bool) {
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return false, false
	}
	var rel string
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			rel = p
		}
	}
	if rel == "" {
		return false, false
	}
	for dir := filepath.Join("/sys/fs/cgroup", rel); strings.HasPrefix(dir, "/sys/fs/cgroup/"); dir = filepath.Dir(dir) {
		if b, err := os.ReadFile(filepath.Join(dir, "cpu.max")); err == nil && !strings.HasPrefix(strings.TrimSpace(string(b)), "max") { //nolint:gosec // cgroup file
			cpu = true
		}
		if b, err := os.ReadFile(filepath.Join(dir, "memory.max")); err == nil && strings.TrimSpace(string(b)) != "max" { //nolint:gosec // cgroup file
			mem = true
		}
	}
	return cpu, mem
}
