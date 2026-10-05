package secret

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Peer checks that the other end of c is this user's process running this
// very binary: the keeper's socket path is the person's, but a process that
// took the path first must not get a session.
func Peer(c net.Conn) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return errors.New("not a Unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if credErr != nil {
		return credErr
	}
	if int(cred.Uid) != os.Getuid() {
		return fmt.Errorf("the listener runs as uid %d, not as this user", cred.Uid)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	peer, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(int(cred.Pid)), "exe"))
	if err != nil {
		return fmt.Errorf("the listener's binary: %w", err)
	}
	if peer = strings.TrimSuffix(peer, " (deleted)"); peer != exe {
		return fmt.Errorf("the listener (pid %d) runs %s, not this beekeeper (%s): restart beekeeper-sandbox.service after an update", cred.Pid, peer, exe)
	}
	return nil
}

// Protect makes this process undumpable: no other process of the user
// reads its memory or environment through /proc or ptrace, and it leaves no
// core dump with the session in it.
func Protect() error { return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0) }
