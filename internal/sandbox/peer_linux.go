package sandbox

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SameUser refuses a loopback connection whose peer socket another user
// owns: the proxy adds the GitHub token, so it serves the broker's own
// user (Claude Code's bridge to the sandbox) and no other account on the
// machine. The owner comes from the kernel's socket table in procDir.
func SameUser(procDir string) func(net.Conn) error {
	return func(c net.Conn) error {
		uid, err := peerUID(procDir, c)
		if err != nil {
			return err
		}
		if uid != os.Getuid() {
			return fmt.Errorf("its socket belongs to uid %d", uid)
		}
		return nil
	}
}

// peerUID is the owner of c's peer socket: the row of the socket table
// whose local address is c's remote one and whose remote address is c's
// local one.
func peerUID(procDir string, c net.Conn) (int, error) {
	local, ok1 := c.LocalAddr().(*net.TCPAddr)
	remote, ok2 := c.RemoteAddr().(*net.TCPAddr)
	if !ok1 || !ok2 {
		return 0, fmt.Errorf("not a TCP connection")
	}
	for _, table := range []string{"tcp", "tcp6"} {
		f, err := os.Open(filepath.Join(procDir, "net", table)) //nolint:gosec // the kernel's socket table
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 8 || fields[0] == "sl" {
				continue
			}
			if procAddrIs(fields[1], remote) && procAddrIs(fields[2], local) {
				_ = f.Close()
				return strconv.Atoi(fields[7])
			}
		}
		_ = f.Close()
	}
	return 0, fmt.Errorf("no socket of %s in the socket table", remote)
}

// procAddrIs reports whether a socket table address (hex IP in host byte
// order per 32-bit word, colon, hex port) is a.
func procAddrIs(field string, a *net.TCPAddr) bool {
	h, p, ok := strings.Cut(field, ":")
	if !ok {
		return false
	}
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil || int(port) != a.Port {
		return false
	}
	raw, err := hex.DecodeString(h)
	if err != nil || len(raw)%4 != 0 {
		return false
	}
	for i := 0; i < len(raw); i += 4 {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	return net.IP(raw).Equal(a.IP)
}
