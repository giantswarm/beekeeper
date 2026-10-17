//go:build !linux

package guard

// onTmpfs reports false off Linux, whose /tmp is a disk directory.
func onTmpfs(string) bool { return false }
