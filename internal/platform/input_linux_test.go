//go:build linux && !nosystemd

package platform

import (
	"testing"
	"time"
)

// The input watch starts at its own start, and says why when it can read no
// keyboard or pointer (a CI runner has none).
func TestEvdevInputWatch(t *testing.T) {
	before := time.Now()
	last, err := evdevInput{}.Watch(t.Context())
	if err != nil {
		t.Skip(err)
	}
	if l := last(); l.Before(before) || l.After(time.Now()) {
		t.Errorf("last input %s, want the watch's start", l)
	}
}

// A name with no capabilities under /sys/class/input is no device of the
// person's.
func TestPersonDevice(t *testing.T) {
	if personDevice("event-none") {
		t.Error("a device without capabilities counts")
	}
}
