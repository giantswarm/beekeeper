//go:build !linux || nosystemd

package platform

import "runtime"

// Name is the platform this build runs on: the system the stub stands in
// for.
func Name() string {
	if runtime.GOOS == "linux" {
		return "linux without systemd"
	}
	return runtime.GOOS
}

// current is the stub: every part is not available but the system's own
// service manager for the standby service, where it has one.
func current(Options) Platform {
	p := Stub()
	p.Setup = nativeSetup()
	return p
}
