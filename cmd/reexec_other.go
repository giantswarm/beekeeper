//go:build !linux

package cmd

// binary is a no-op off Linux: a waiting gate call keeps its binary.
type binary struct{ path string }

func runningBinary() *binary { return nil }

func (*binary) replaced() bool { return false }

func (*binary) exec(...string) error { return nil }
