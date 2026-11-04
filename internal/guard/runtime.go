package guard

import (
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const runtimeLintCommand = "golangci-lint"

var lintCommand = regexp.MustCompile(`(?m)` + pos + `(golangci-lint)\b`)

// RunTimeout bounds local slot occupancy, independently of the queue wait.
// Slotless lab proofs and benchmarks retain their unbounded runtime unless
// the caller supplies a deadline.
func RunTimeout(o Options, argv []string) time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}
	if o.slotless() {
		return 0
	}
	if commandMatch(lintCommand, shellCommand(argv), notHelp) != nil {
		return 5 * time.Minute
	}
	return 30 * time.Minute
}

// lintTimeout supplies the linter's own deadline for direct invocations.
// Compound shell commands are bounded by the enclosing scope deadline.
func lintTimeout(argv []string, deadline time.Duration) []string {
	if deadline <= 0 || len(argv) < 2 || filepath.Base(argv[0]) != runtimeLintCommand || argv[1] != executionRunCommand {
		return argv
	}
	for _, arg := range argv[2:] {
		if arg == "--timeout" || strings.HasPrefix(arg, "--timeout=") {
			return argv
		}
	}
	out := append([]string{}, argv[:2]...)
	out = append(out, "--timeout="+deadline.String())
	return append(out, argv[2:]...)
}
