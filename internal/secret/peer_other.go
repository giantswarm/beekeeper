//go:build !linux

package secret

import "net"

// Peer checks nothing beyond the socket's mode outside Linux, which has no
// SO_PEERCRED: the 0600 socket in a 0700 directory admits this user only.
func Peer(net.Conn) error { return nil }

// Protect does nothing outside Linux.
func Protect() error { return nil }
