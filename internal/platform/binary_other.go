//go:build !linux

package platform

// Binary is a no-op off Linux: a waiting gate call keeps its binary.
type Binary struct{ Path string }

// RunningBinary is nil off Linux.
func RunningBinary() *Binary { return nil }

// Replaced is false off Linux.
func (*Binary) Replaced() bool { return false }

// Exec does nothing off Linux.
func (*Binary) Exec(...string) error { return nil }
