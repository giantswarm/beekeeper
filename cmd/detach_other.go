//go:build !unix

package cmd

import "os/exec"

// detach does nothing where there are no sessions.
func detach(*exec.Cmd) {}
