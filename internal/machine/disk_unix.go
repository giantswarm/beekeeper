//go:build unix

package machine

import "golang.org/x/sys/unix"

// Disk is a filesystem's usage in MiB.
type Disk struct {
	Path    string `json:"path"`
	UsedMiB int    `json:"usedMiB"`
	FreeMiB int    `json:"freeMiB"`
}

// ReadDisk returns the usage of the filesystem path lives on; free is what an
// unprivileged process can still write.
func ReadDisk(path string) (Disk, error) {
	var s unix.Statfs_t
	if err := unix.Statfs(path, &s); err != nil {
		return Disk{}, err
	}
	bs := uint64(s.Bsize) //nolint:gosec // block sizes are positive
	return Disk{
		Path:    path,
		UsedMiB: int((s.Blocks - s.Bfree) * bs >> 20),
		FreeMiB: int(s.Bavail * bs >> 20),
	}, nil
}
