package machine

import (
	"encoding/hex"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// tcpEstablished is the state column of an established connection in
// /proc/net/tcp.
const tcpEstablished = "01"

// tcpConn is one row of /proc/net/tcp or tcp6. An IPv4 address of a
// dual-stack socket reads as IPv4.
type tcpConn struct {
	local, remote netip.AddrPort
	state, inode  string
}

// establishedPeers is the address of every client connected to the local
// port, from the host's /proc/net/tcp and tcp6, each once and sorted. A
// client that is a beekeeper process (this one or another, of any version)
// asking the server about its models is left out.
func establishedPeers(port int) []string {
	var conns []tcpConn
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		raw, err := os.ReadFile(f) //nolint:gosec // fixed procfs paths
		if err != nil {
			continue
		}
		conns = append(conns, parseTCPTable(string(raw))...)
	}
	return peersOf(conns, port, processSockets("/proc", "beekeeper"))
}

// peersOf is the remote address of every established connection to port,
// once and sorted, but for those whose client socket is one of skip.
func peersOf(conns []tcpConn, port int, skip map[string]bool) []string {
	inodeAt := map[netip.AddrPort]string{}
	for _, c := range conns {
		inodeAt[c.local] = c.inode
	}
	var out []string
	for _, c := range conns {
		if c.state != tcpEstablished || int(c.local.Port()) != port || skip[inodeAt[c.remote]] {
			continue
		}
		out = append(out, c.remote.Addr().String())
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// parseTCPTable reads the rows of one /proc/net/tcp or tcp6 table.
func parseTCPTable(table string) []tcpConn {
	var out []tcpConn
	for line := range strings.SplitSeq(table, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 {
			continue
		}
		local, lok := procAddrPort(f[1])
		remote, rok := procAddrPort(f[2])
		if !lok || !rok {
			continue
		}
		out = append(out, tcpConn{local: local, remote: remote, state: f[3], inode: f[9]})
	}
	return out
}

// procAddrPort decodes an address and port as procfs prints them: 32-bit
// words in hex, each in the host's (little-endian) byte order, then the
// port in hex.
func procAddrPort(s string) (netip.AddrPort, bool) {
	a, p, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, false
	}
	b, err := hex.DecodeString(a)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return netip.AddrPort{}, false
	}
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil {
		return netip.AddrPort{}, false
	}
	for i := 0; i < len(b); i += 4 {
		slices.Reverse(b[i : i+4])
	}
	addr, _ := netip.AddrFromSlice(b)
	return netip.AddrPortFrom(addr.Unmap(), uint16(port)), true
}

// processSockets is the socket inodes the processes named comm hold open,
// of those under proc this user may read.
func processSockets(proc, comm string) map[string]bool {
	out := map[string]bool{}
	comms, _ := filepath.Glob(filepath.Join(proc, "[0-9]*", "comm"))
	for _, c := range comms {
		name, err := os.ReadFile(c) //nolint:gosec // procfs
		if err != nil || strings.TrimSpace(string(name)) != comm {
			continue
		}
		fds, _ := filepath.Glob(filepath.Join(filepath.Dir(c), "fd", "*"))
		for _, fd := range fds {
			if t, err := os.Readlink(fd); err == nil {
				if inode, ok := strings.CutPrefix(t, "socket:["); ok {
					out[strings.TrimSuffix(inode, "]")] = true
				}
			}
		}
	}
	return out
}
