//go:build !linux

package secret

import (
	"context"
	"errors"
	"net"
)

// errNoBroker says BrokerUnit is a systemd one.
var errNoBroker = errors.New("no broker unit outside Linux")

// Peer checks nothing beyond the socket's mode outside Linux, which has no
// SO_PEERCRED: the 0600 socket in a 0700 directory admits this user only.
func Peer(net.Conn) error { return nil }

// Protect does nothing outside Linux.
func Protect() error { return nil }

// BrokerProcess answers no broker outside Linux.
func BrokerProcess() (int, string, error) { return 0, "", errNoBroker }

// RestartBroker has no broker to restart outside Linux.
func RestartBroker(context.Context) error { return errNoBroker }
