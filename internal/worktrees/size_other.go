//go:build !unix

package worktrees

import "io/fs"

// allocated is fi's apparent size: the platform says no blocks.
func allocated(fi fs.FileInfo) int64 {
	if fi.Mode().IsRegular() {
		return fi.Size()
	}
	return 0
}
