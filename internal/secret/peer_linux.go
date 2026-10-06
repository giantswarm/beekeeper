package secret

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// BrokerUnit is the systemd user unit whose main process is the broker.
const BrokerUnit = "beekeeper-sandbox.service"

// Peer checks that the other end of c is this user's broker: the main
// process of BrokerUnit, whose unit runs this very binary. The keeper's
// socket path is the person's, but a process that took the path first must
// not get a session. The broker is undumpable (Protect), so its
// /proc/<pid>/exe belongs to root and tells nothing; systemd names the
// process and the binary instead.
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
	pid, bin, err := brokerProcess()
	if err != nil {
		return fmt.Errorf("%s: %w", BrokerUnit, err)
	}
	if int(cred.Pid) != pid {
		return fmt.Errorf("the listener (pid %d) is not %s's main process (pid %d)", cred.Pid, BrokerUnit, pid)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved(bin) != resolved(exe) {
		return fmt.Errorf("%s runs %s, not this beekeeper (%s): beekeeper install points it at this binary", BrokerUnit, bin, exe)
	}
	return nil
}

// brokerProcess answers the broker unit's main process and the binary its
// unit starts.
var brokerProcess = func() (int, string, error) {
	out, err := exec.Command("systemctl", "--user", "show", BrokerUnit, "--property=MainPID", "--property=ExecStart").Output()
	if err != nil {
		return 0, "", fmt.Errorf("systemctl show: %w", err)
	}
	return parseUnitShow(string(out))
}

// execPath is the binary of systemctl show's ExecStart value.
var execPath = regexp.MustCompile(`(?:^|[{;]\s*)path=(\S+)`)

// parseUnitShow reads MainPID and ExecStart's binary from systemctl show.
func parseUnitShow(out string) (int, string, error) {
	var pid int
	var bin string
	for line := range strings.Lines(out) {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "MainPID":
			pid, _ = strconv.Atoi(v)
		case "ExecStart":
			if m := execPath.FindStringSubmatch(v); m != nil {
				bin = m[1]
			}
		}
	}
	if pid <= 0 {
		return 0, "", errors.New("not running: beekeeper install starts it")
	}
	if bin == "" {
		return 0, "", errors.New("no ExecStart binary")
	}
	return pid, bin, nil
}

// resolved is path with its symbolic links resolved, else path.
func resolved(path string) string {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	return path
}

// Protect makes this process undumpable: no other process of the user
// reads its memory or environment through /proc or ptrace, and it leaves no
// core dump with the session in it.
func Protect() error { return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0) }
