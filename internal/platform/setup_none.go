//go:build !darwin && (!linux || nosystemd)

package platform

// nativeSetup is none: this system has no service manager beekeeper
// installs the standby service with.
func nativeSetup() Setup { return stubSetup{} }
