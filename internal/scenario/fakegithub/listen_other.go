//go:build scenario && !linux

package fakegithub

import "net"

// listenLoopback listens on a free loopback port.
func listenLoopback() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }
