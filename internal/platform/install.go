package platform

import "time"

// Install is the executable an install (beekeeper self-update, go install)
// renamed over a running binary's path: that path and when the file arrived
// there, zero where the platform cannot say.
type Install struct {
	Path string
	At   time.Time
}
