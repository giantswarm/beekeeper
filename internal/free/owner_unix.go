//go:build unix

package free

import (
	"os"
	"syscall"
)

// fileUID returns the owner of fi.
func fileUID(fi os.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}
