package guard

import "golang.org/x/sys/unix"

// onTmpfs reports whether path lies on a tmpfs, whose files take RAM. A
// path that cannot be read counts as tmpfs: the run gives it its own
// directory.
func onTmpfs(path string) bool {
	var s unix.Statfs_t
	if err := unix.Statfs(path, &s); err != nil {
		return true
	}
	return s.Type == unix.TMPFS_MAGIC
}
