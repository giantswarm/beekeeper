//go:build !linux

package secret

import "net"

// askpassPeer checks nothing beyond the socket's 0700 directory outside
// Linux, which has no SO_PEERCRED.
func askpassPeer(net.Conn, int) error { return nil }
