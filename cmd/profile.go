package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
)

// cpuProfile writes the process's CPU profile to file until the returned stop
// runs: a long-running command's cost (a watch's ticks) measured on the
// machine it runs on, with `go tool pprof`. An empty file profiles nothing.
func cpuProfile(file string) (stop func()) {
	if file == "" {
		return func() {}
	}
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
