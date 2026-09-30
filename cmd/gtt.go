package cmd

import (
	"fmt"
	"strings"

	"github.com/giantswarm/beekeeper/internal/machine"
)

// gttLine says how much RAM the iGPU pins and which of the host's model
// servers' models hold it, or "" on a machine without an amdgpu device.
func gttLine(gpus []machine.GPU, models []machine.HostModel) string {
	if len(gpus) == 0 {
		return ""
	}
	total, vram, vramTotal := 0, 0, 0
	for _, g := range gpus {
		total += g.GTTTotalMiB
		vram += g.VRAMUsedMiB
		vramTotal += g.VRAMTotMiB
	}
	line := fmt.Sprintf("iGPU GTT %d of %d MiB (in no cgroup), VRAM %d of %d MiB", machine.GTTUsedMiB(gpus), total, vram, vramTotal)
	if len(models) == 0 {
		return line + "; the model servers hold no model"
	}
	var ms []string
	for _, m := range models {
		s := fmt.Sprintf("%s %d MiB", m.Label(), m.SizeMiB)
		if m.Client != "" {
			s += " loaded by " + m.Client
		}
		ms = append(ms, s)
	}
	return line + "; the model servers hold " + strings.Join(ms, ", ")
}

// because appends a named cause to a memory line.
func because(cause string) string {
	if cause == "" {
		return ""
	}
	return "; cause: " + cause
}
