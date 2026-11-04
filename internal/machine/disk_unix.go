//go:build unix

package machine

import "golang.org/x/sys/unix"

// Disk is a filesystem's usage in MiB.
type Disk struct {
	Path    string `json:"path"`
	UsedMiB int    `json:"usedMiB"`
	FreeMiB int    `json:"freeMiB"`
	// TotalMiB is the filesystem's size.
	TotalMiB int `json:"totalMiB"`
}

// ReadDisk returns the usage of the filesystem path lives on; free is what an
// unprivileged process can still write.
func ReadDisk(path string) (Disk, error) {
	var s unix.Statfs_t
	if err := unix.Statfs(path, &s); err != nil {
		return Disk{}, err
	}
	return diskUsage(path, &s), nil
}

func diskUsage(path string, s *unix.Statfs_t) Disk {
	bs := uint64(s.Bsize) //nolint:gosec // block sizes are positive
	// FreeBSD reports signed availability, which can be negative when
	// reserved space is exhausted. Linux and macOS use an unsigned count.
	var available uint64
	if s.Bavail > 0 {
		available = uint64(s.Bavail)
	}
	return Disk{
		Path:     path,
		UsedMiB:  int((s.Blocks - s.Bfree) * bs >> 20),
		FreeMiB:  int(available * bs >> 20),
		TotalMiB: int(s.Blocks * bs >> 20),
	}
}
