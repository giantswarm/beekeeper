//go:build !linux

package sandbox

import (
	"errors"
	"net"
)

// SameUser is the broker's, on Linux only.
func SameUser(string) func(net.Conn) error {
	return func(net.Conn) error { return errors.New("the sandbox broker runs on Linux only") }
}
