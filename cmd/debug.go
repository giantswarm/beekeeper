package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/giantswarm/beekeeper/internal/sandbox"
)

// debugEnv, set to anything, has beekeeper say on stderr what it decides
// about its own environment before the command runs.
const debugEnv = "BEEKEEPER_DEBUG"

// debugf writes one debug line to w when debugEnv is set.
func debugf(w io.Writer, format string, args ...any) {
	if os.Getenv(debugEnv) == "" {
		return
	}
	_, _ = fmt.Fprintf(w, "beekeeper: debug: "+format+"\n", args...)
}

// unconfineUnheld drops the sandbox policy's variables from an unheld
// environment (sandbox.Unheld): beekeeper and what it runs act as on the
// host, a brokered call included, which then runs locally. The debug line
// names the drop, so a proof's transcript shows a client that went local.
func unconfineUnheld(w io.Writer) {
	if !sandbox.Unheld(os.Getenv) {
		return
	}
	dir := sandbox.EgressDir(os.Getenv("XDG_RUNTIME_DIR"))
	debugf(w, "%s set without %s: the sandbox policy's environment without its sandbox; unset %s, this command runs on the host, not through the sandbox broker",
		sandbox.Env, sandbox.Runtime, strings.Join(sandbox.Unset(dir), " "))
	sandbox.Unconfine(dir)
}
