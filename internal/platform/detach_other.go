//go:build !unix

package platform

import "os/exec"

// Detach does nothing where there are no sessions.
func Detach(*exec.Cmd) {}
