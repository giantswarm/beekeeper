package cmd

import (
	"strconv"
	"strings"
)

// squashArgv is the gate's command for repo#pr on the plain squash merge
// route, with the caller's --timeout.
func squashArgv(self, repo string, pr int, devctlArgv []string) []string {
	argv := []string{self, squashMergeCmd, repo, strconv.Itoa(pr)}
	for i, a := range devctlArgv {
		if v, ok := strings.CutPrefix(a, "--timeout="); ok {
			return append(argv, "--timeout", v)
		}
		if a == "--timeout" && i+1 < len(devctlArgv) {
			return append(argv, "--timeout", devctlArgv[i+1])
		}
	}
	return argv
}
