//go:build !unix

package alerts

import "os/exec"

// ownGroup leaves cmd to exec's default cancel, which kills the process.
func ownGroup(*exec.Cmd) {}
