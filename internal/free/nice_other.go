//go:build unix && !linux

package free

import "syscall"

// Nice lowers this process's priority to n.
func Nice(n int) { _ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, n) }
