//go:build !unix

package cmd

import "os/exec"

// mergeCheckGroup leaves c as it is where there are no process groups.
func mergeCheckGroup(*exec.Cmd) {}
