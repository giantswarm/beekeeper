//go:build !unix

package codex

import "os/exec"

func configureTurnCmd(*exec.Cmd) {}

func stopTurnProcessGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
