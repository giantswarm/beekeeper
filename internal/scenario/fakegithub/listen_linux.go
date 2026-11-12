//go:build scenario && linux

package fakegithub

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// listenLoopback listens on a free loopback port. A scenario's sandbox is
// a network namespace of its own, whose loopback interface starts down
// and without an address: it is brought up first, which the namespace's
// owner may do.
func listenLoopback() (net.Listener, error) {
	upErr := loopbackUp()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil && upErr != nil {
		return nil, fmt.Errorf("listen on loopback: %w (bringing lo up: %v)", err, upErr)
	}
	return ln, err
}

// loopbackUp sets IFF_UP on lo unless it is up; the kernel then gives it
// 127.0.0.1.
func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	if ifr.Uint16()&unix.IFF_UP != 0 {
		return nil
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}
