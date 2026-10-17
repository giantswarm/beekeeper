//go:build unix

package worktrees

import (
	"io/fs"
	"syscall"
)

// allocated is the disk space fi's file takes: its 512-byte blocks.
func allocated(fi fs.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return fi.Size()
}
