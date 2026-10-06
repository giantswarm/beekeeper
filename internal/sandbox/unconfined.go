package sandbox

import (
	"maps"
	"os"
	"slices"
	"strings"
)

// The variables the egress login is given by.
const (
	ghConfigDir    = "GH_CONFIG_DIR"
	gitConfigCount = "GIT_CONFIG_COUNT"
	gitHubHelper   = "credential.https://github.com.helper"
	sslCertFile    = "SSL_CERT_FILE"
)

// Runtime is set by the sandbox runtime in every command it holds; the
// host never has it.
const Runtime = "SANDBOX_RUNTIME"

// Unheld reports whether an environment carries the policy's variables
// while no sandbox holds it: Claude Code applied the policy's env but not
// its sandbox block, so no egress proxy completes the egress login. A
// brokered call is held by the broker instead.
func Unheld(getenv func(string) string) bool {
	return getenv(Env) != "" && getenv(Brokered) == "" && getenv(Runtime) == ""
}

// Unset are the variables an unheld environment drops to be the host's
// again: the policy's egress variables and Env.
func Unset(egressDir string) []string {
	return append(slices.Sorted(maps.Keys(egressVars(egressDir))), Env)
}

// Unconfine drops Unset from this process's environment.
func Unconfine(egressDir string) {
	for _, k := range Unset(egressDir) {
		_ = os.Unsetenv(k)
	}
}

// UnsetShell is Unset as a POSIX shell line.
func UnsetShell(egressDir string) string {
	return "unset " + strings.Join(Unset(egressDir), " ") + "\n"
}
