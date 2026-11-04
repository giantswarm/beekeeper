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
	return diskFromStatfs(path, s), nil
}

func diskFromStatfs(path string, s unix.Statfs_t) Disk {
	bs := uint64(s.Bsize) //nolint:gosec // block sizes are positive
	var available uint64
	// Bavail is signed on FreeBSD and can be negative when space is exhausted.
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
