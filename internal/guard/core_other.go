//go:build !unix

package guard

// coreDumpsOn is false where the platform has no core dump limit.
func coreDumpsOn() bool { return false }
