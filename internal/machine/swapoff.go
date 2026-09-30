package machine

import (
	"os"
	"path/filepath"
	"strings"
)

// SwapoffRuns reports whether a swapoff process runs. While swapoff drains
// a device the kernel has already taken the device out of SwapTotal, but
// its pages still count as used, so swap reads near 100 % full while it is
// being emptied.
func SwapoffRuns() bool {
	return swapoffIn("/proc")
}

func swapoffIn(procDir string) bool {
	comms, _ := filepath.Glob(filepath.Join(procDir, "[0-9]*", "comm"))
	for _, c := range comms {
		if b, err := os.ReadFile(c); err == nil && strings.TrimSpace(string(b)) == "swapoff" { //nolint:gosec // /proc/<pid>/comm
			return true
		}
	}
	return false
}
