//go:build !linux

package secret

import "testing"

// asBroker needs nothing outside Linux, where Peer checks nothing.
func asBroker(*testing.T) {}
