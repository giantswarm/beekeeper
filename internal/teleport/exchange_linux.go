//go:build linux

package teleport

import "golang.org/x/sys/unix"

// exchange swaps the directories a and b in one step.
func exchange(a, b string) error {
	return unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
}
