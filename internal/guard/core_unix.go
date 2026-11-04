//go:build unix

package guard

import "syscall"

// coreDumpsOn reports whether either inherited core dump limit is nonzero.
func coreDumpsOn() bool {
	var inherited syscall.Rlimit
	return syscall.Getrlimit(syscall.RLIMIT_CORE, &inherited) == nil && (inherited.Cur != 0 || inherited.Max != 0)
}
