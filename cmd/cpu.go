package cmd

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// topCPUCommands is how many commands a CPU line names.
const topCPUCommands = 5

// cpuUse is the processor time one command name burned over a span, summed
// over its processes.
type cpuUse struct {
	Comm      string
	Cores     float64
	Processes int
}

// topCPU groups the CPU time each process of cur burned since prev, read
// span earlier, by command name, busiest first, at most n. A process prev
// has not seen (a new PID, or a reused one) counts its whole CPU time.
func topCPU(prev, cur *proc.Table, span time.Duration, n int) []cpuUse {
	if prev == nil || cur == nil || span <= 0 {
		return nil
	}
	by := map[string]*cpuUse{}
	for pid, p := range cur.ByPID {
		burned := p.CPU
		if q := prev.ByPID[pid]; q != nil && q.StartTicks == p.StartTicks {
			burned -= q.CPU
		}
		if burned <= 0 {
			continue
		}
		u := by[p.Comm]
		if u == nil {
			u = &cpuUse{Comm: p.Comm}
			by[p.Comm] = u
		}
		u.Cores += burned.Seconds() / span.Seconds()
		u.Processes++
	}
	uses := make([]cpuUse, 0, len(by))
	for _, u := range by {
		uses = append(uses, *u)
	}
	slices.SortFunc(uses, func(a, b cpuUse) int {
		return cmp.Or(cmp.Compare(b.Cores, a.Cores), cmp.Compare(a.Comm, b.Comm))
	})
	return uses[:min(n, len(uses))]
}

// topCPULine is the tail of a CPU line: "; top CPU over 30s: go 11.8 cores
// (8 processes), …", or "" before a second process-table read.
func topCPULine(uses []cpuUse, span time.Duration) string {
	if len(uses) == 0 {
		return ""
	}
	parts := make([]string, len(uses))
	for i, u := range uses {
		parts[i] = fmt.Sprintf("%s %.1f cores (%d)", u.Comm, u.Cores, u.Processes)
	}
	return fmt.Sprintf("; top CPU over %s: %s", span.Round(time.Second), strings.Join(parts, ", "))
}

// loadRising reports a steep climb before saturation: the 1-minute load
// over one per core and more than twice the 5-minute one.
func loadRising(load [3]float64, cores int) bool {
	return load[0] > float64(cores) && load[0] > 2*load[1]
}
