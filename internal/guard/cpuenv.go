package guard

import (
	"os"
	"strconv"
	"strings"
)

// CPUEnv are the variables that bound a command's own parallelism to its
// share of the cores (cpuEnv): the go command's build and test parallelism,
// a Go program's threads (golangci-lint's concurrency follows them) and
// cargo's jobs.
var CPUEnv = []string{"GOMAXPROCS", "CARGO_BUILD_JOBS"}

// SlotCores is a slot's share of memcap.slice's CPUQuota: the quota's whole
// cores ("1200%" is twelve) over the slots, at least one; 0 when the quota
// sets no bound ("", "max", unparsable) or there are no slots.
func SlotCores(quota string, slots int) int {
	pct, ok := strings.CutSuffix(strings.TrimSpace(quota), "%")
	if !ok || slots < 1 {
		return 0
	}
	p, err := strconv.ParseFloat(pct, 64)
	if err != nil || p <= 0 {
		return 0
	}
	return max(int(p/100)/slots, 1)
}

// cpuEnv bounds the command's parallelism to its slot's share of the cores
// (SlotCores): each of CPUEnv the caller does not set itself. The quota
// bounds the cores the runs use, not the threads they start: a linter or a
// compile with a thread or a job per core keeps every core's run queue
// full between its throttled periods, which raises the machine's load far
// beyond the cores it gets and holds every start the load gates.
func cpuEnv(o Options) []string {
	n := SlotCores(o.CPUQuota, o.Slots)
	if n == 0 {
		return nil
	}
	var env []string
	for _, k := range CPUEnv {
		if os.Getenv(k) == "" {
			env = append(env, k+"="+strconv.Itoa(n))
		}
	}
	return env
}
