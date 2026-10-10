package secret

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// askpassPeer checks that the other end of c is this user's process
// descending from git (pid): the helper git or its remote helper runs, not
// another process that found the socket while git ran.
func askpassPeer(c net.Conn, pid int) error {
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
		return fmt.Errorf("the helper runs as uid %d, not as this user", cred.Uid)
	}
	p := int(cred.Pid)
	for range 16 {
		if p == pid {
			return nil
		}
		if p <= 1 {
			break
		}
		if p, err = parentPid(p); err != nil {
			return err
		}
	}
	return fmt.Errorf("pid %d does not descend from git (pid %d)", cred.Pid, pid)
}

// parentPid reads a process's parent from /proc/<pid>/stat: the field after
// the state, past the command name's closing parenthesis.
func parentPid(pid int) (int, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(f) < 2 {
		return 0, fmt.Errorf("/proc/%d/stat: no parent", pid)
	}
	return strconv.Atoi(f[1])
}
