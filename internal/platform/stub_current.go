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

// current is the stub: every part is not available.
func current(Options) Platform { return Stub() }
