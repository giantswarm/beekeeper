package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
)

// cpuProfileEnv names the file cpuProfile writes.
const cpuProfileEnv = "BEEKEEPER_CPUPROFILE"

// cpuProfile writes the process's CPU profile to file until the returned stop
// runs: a long-running command's cost (a watch's ticks) measured on the
// machine it runs on, with `go tool pprof`. An empty file profiles nothing.
// The profile is this process's: the variable is cleared, so a beekeeper it
// starts (a loop's probe) does not truncate the same file.
func cpuProfile(file string) (stop func()) {
	if file == "" {
		return func() {}
	}
	_ = os.Unsetenv(cpuProfileEnv)
	f, err := os.Create(filepath.Clean(file)) //nolint:gosec // the person names the file in their environment
	if err != nil {
		fmt.Fprintf(os.Stderr, "beekeeper: cpu profile: %v\n", err)
		return func() {}
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		fmt.Fprintf(os.Stderr, "beekeeper: cpu profile: %v\n", err)
		_ = f.Close()
		return func() {}
	}
	return func() {
		pprof.StopCPUProfile()
		_ = f.Close()
	}
}
