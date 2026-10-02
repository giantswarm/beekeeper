package machine

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// OOMDSwap is what systemd-oomd does about swap: its SwapUsedLimit and the
// cgroups it watches for it (ManagedOOMSwap=kill). Past the limit it kills
// the largest swap user among them, however much RAM is free; with none
// watched it never kills for swap.
type OOMDSwap struct {
	// LimitPercent is SwapUsedLimit in whole percent of SwapTotal.
	LimitPercent int `json:"limitPercent"`
	// Monitored are the swap-monitored cgroups' paths.
	Monitored []string `json:"monitored,omitempty"`
}

// Watched reports whether oomd would kill for swap at all.
func (o OOMDSwap) Watched() bool { return len(o.Monitored) > 0 }

// HeadroomMiB is the swap growth left before the trigger; negative once
// swap is past it. oomd counts every swap slot in use, zswap's included.
func (o OOMDSwap) HeadroomMiB(m Mem) int {
	return m.SwapTotalMiB*o.LimitPercent/100 - m.SwapUsedMiB
}

// Line says oomd's swap rule against m: the trigger and its headroom when
// a cgroup is swap-monitored, otherwise that oomd does not watch swap.
func (o OOMDSwap) Line(m Mem) string {
	if !o.Watched() {
		return "systemd-oomd watches no cgroup for swap"
	}
	return fmt.Sprintf("%d MiB before systemd-oomd's %d %% swap trigger (%d swap-monitored cgroups)", o.HeadroomMiB(m), o.LimitPercent, len(o.Monitored))
}

// ReadOOMDSwap asks systemd-oomd for its swap rule (`oomctl dump`); an
// error when oomd does not answer.
func ReadOOMDSwap(ctx context.Context) (OOMDSwap, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "oomctl", "dump").Output()
	if err != nil {
		return OOMDSwap{}, fmt.Errorf("oomctl dump: %w", err)
	}
	return ParseOomctlDump(string(out))
}

// ParseOomctlDump reads the "Swap Used Limit" and the paths under "Swap
// Monitored CGroups" of an `oomctl dump`.
func ParseOomctlDump(dump string) (OOMDSwap, error) {
	var o OOMDSwap
	limit, inSwap := false, false
	for line := range strings.Lines(dump) {
		line = strings.TrimRight(line, "\n")
		if !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, " ") {
			inSwap = line == "Swap Monitored CGroups:"
			if v, ok := strings.CutPrefix(line, "Swap Used Limit:"); ok {
				o.LimitPercent, limit = parsePercent(strings.TrimSpace(v))
			}
			continue
		}
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "Path:"); ok && inSwap {
			o.Monitored = append(o.Monitored, strings.TrimSpace(p))
		}
	}
	if !limit {
		return OOMDSwap{}, fmt.Errorf("oomctl dump: no Swap Used Limit")
	}
	return o, nil
}

// parsePercent reads "90%", "90.00%", "900‰" or "9000‱" as whole percent.
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
