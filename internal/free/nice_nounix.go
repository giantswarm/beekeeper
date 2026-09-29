//go:build !unix

package free

// Nice does nothing off Unix.
func Nice(int) {}
