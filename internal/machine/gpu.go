package machine

import (
	"path/filepath"
	"slices"
)

// GPU is an amdgpu device's memory in MiB. On an APU, GTT is system RAM the
// driver pins for the GPU: no cgroup counts it, but it is gone from
// MemAvailable all the same.
type GPU struct {
	Device      string `json:"device"`
	GTTUsedMiB  int    `json:"gttUsedMiB"`
	GTTTotalMiB int    `json:"gttTotalMiB"`
	VRAMUsedMiB int    `json:"vramUsedMiB"`
	VRAMTotMiB  int    `json:"vramTotalMiB"`
}

// drmGlob matches the amdgpu devices' GTT counters; the connectors
// (card1-DP-1, …) have no device memory files and do not match.
const drmGlob = "/sys/class/drm/card*/device/mem_info_gtt_used"

// ReadGPUs returns every amdgpu device's GTT and VRAM, none on a machine
// without one.
func ReadGPUs() []GPU {
	return readGPUs(drmGlob)
}

func readGPUs(glob string) []GPU {
	m, _ := filepath.Glob(glob)
	slices.Sort(m)
	var out []GPU
	for _, used := range m {
		dir := filepath.Dir(used)
		out = append(out, GPU{
			Device:      filepath.Base(filepath.Dir(dir)),
			GTTUsedMiB:  int(readInt(used) >> 20),
			GTTTotalMiB: int(readInt(filepath.Join(dir, "mem_info_gtt_total")) >> 20),
			VRAMUsedMiB: int(readInt(filepath.Join(dir, "mem_info_vram_used")) >> 20),
			VRAMTotMiB:  int(readInt(filepath.Join(dir, "mem_info_vram_total")) >> 20),
		})
	}
	return out
}

// GTTUsedMiB is the GTT all devices pin.
func GTTUsedMiB(gpus []GPU) int {
	n := 0
	for _, g := range gpus {
		n += g.GTTUsedMiB
	}
	return n
}
