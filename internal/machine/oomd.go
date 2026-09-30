package machine

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// DefaultOOMDSwapLimit is systemd-oomd's SwapUsedLimit when no oomd.conf
// sets it: the percent of swap in use at which it kills the monitored
// cgroup's largest swap user, however much RAM is free.
const DefaultOOMDSwapLimit = 90

// oomdConfs are oomd.conf and its drop-in directories, lowest precedence
// first, the order systemd reads them in.
var oomdConfs = []string{"/usr/lib/systemd/oomd.conf", "/etc/systemd/oomd.conf"}

var oomdDropins = []string{"/usr/lib/systemd/oomd.conf.d", "/run/systemd/oomd.conf.d", "/etc/systemd/oomd.conf.d"}

// OOMDSwapLimit returns systemd-oomd's SwapUsedLimit in percent: the last
// setting in oomd.conf and its drop-ins, DefaultOOMDSwapLimit without one.
func OOMDSwapLimit() int {
	files := append([]string(nil), oomdConfs...)
	for _, d := range oomdDropins {
		m, _ := filepath.Glob(filepath.Join(d, "*.conf"))
		sort.Strings(m)
		files = append(files, m...)
	}
	limit := DefaultOOMDSwapLimit
	for _, f := range files {
		if p, ok := readSwapUsedLimit(f); ok {
			limit = p
		}
	}
	return limit
}

// readSwapUsedLimit reads the SwapUsedLimit= of one oomd.conf file.
func readSwapUsedLimit(path string) (int, bool) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()
	limit, found := 0, false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok || strings.TrimSpace(k) != "SwapUsedLimit" {
			continue
		}
		v = strings.TrimSpace(v)
		if p, ok := parsePercent(v); ok {
			limit, found = p, true
		}
	}
	return limit, found
}

// parsePercent reads "90%", "90.5%", "900‰" or "9000‱" as whole percent.
func parsePercent(v string) (int, bool) {
	div := 1.0
	switch {
	case strings.HasSuffix(v, "%"):
		v = strings.TrimSuffix(v, "%")
	case strings.HasSuffix(v, "‰"):
		v, div = strings.TrimSuffix(v, "‰"), 10
	case strings.HasSuffix(v, "‱"):
		v, div = strings.TrimSuffix(v, "‱"), 100
	default:
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return int(f / div), true
}

// OOMDHeadroomMiB is the swap growth left before systemd-oomd's trigger at
// limit percent; negative once swap is past it.
func (m Mem) OOMDHeadroomMiB(limit int) int {
	return m.SwapTotalMiB*limit/100 - m.SwapUsedMiB
}
