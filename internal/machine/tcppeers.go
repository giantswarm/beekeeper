package machine

import (
	"encoding/hex"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
)

// tcpEstablished is the state column of an established connection in
// /proc/net/tcp.
const tcpEstablished = "01"

// establishedPeers is the address of every client connected to the local
// port, from the host's /proc/net/tcp and tcp6, each once and sorted. An
// IPv4 client of a dual-stack listener reads as its IPv4 address.
func establishedPeers(port int) []string {
	var out []string
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		raw, err := os.ReadFile(f) //nolint:gosec // fixed procfs paths
		if err != nil {
			continue
		}
		out = append(out, parseTCPPeers(string(raw), port)...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// parseTCPPeers reads the established connections of one /proc/net/tcp or
// tcp6 table whose local port is port and returns their remote addresses.
func parseTCPPeers(table string, port int) []string {
	var out []string
	for line := range strings.SplitSeq(table, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[3] != tcpEstablished {
			continue
		}
		_, lport, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		if p, err := strconv.ParseUint(lport, 16, 16); err != nil || int(p) != port {
			continue
		}
		rip, _, ok := strings.Cut(f[2], ":")
		if !ok {
			continue
		}
		if a, ok := procAddr(rip); ok {
			out = append(out, a.Unmap().String())
		}
	}
	return out
}

// procAddr decodes an address as procfs prints it: 32-bit words in hex, each
// in the host's (little-endian) byte order.
func procAddr(s string) (netip.Addr, bool) {
	b, err := hex.DecodeString(s)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return netip.Addr{}, false
	}
	for i := 0; i < len(b); i += 4 {
		slices.Reverse(b[i : i+4])
	}
	return netip.AddrFromSlice(b)
}
