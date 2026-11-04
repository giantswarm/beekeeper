//go:build !linux

package proc

import (
	"errors"
	"time"
)

// StopIdle needs Linux pidfds and the process table.
func StopIdle(int, time.Time, func() bool) error {
	return errors.New("idle process recovery unavailable on this platform")
}
