//go:build !scenario

package proc

// scenarioCgroup is the scenario build's cgroup of a process; a release
// build has none and reads the kernel's.
func scenarioCgroup(int) (string, bool) { return "", false }
