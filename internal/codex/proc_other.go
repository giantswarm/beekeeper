//go:build !unix

package codex

import "os/exec"

func configureTurnCmd(*exec.Cmd)     {}
func stopTurnProcessGroup(*exec.Cmd) {}
