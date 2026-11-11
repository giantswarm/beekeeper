//go:build perf && linux

package perfsuite

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// Twenty calls at once, as a busy desk's sessions make them: readers of the
// roster, status and notes, and writers of the event log.
var (
	concurrentCalls = 20
	concurrentAll   = limit{Max: 30}  // seconds until the last call ends
	concurrentHold  = limit{Max: 2.0} // seconds the state lock is held exclusively at most
)

func concurrentArgs(i int) []string {
	switch i % 5 {
	case 0, 1:
		return []string{"--as", fmt.Sprintf("perf worker %d", i), "agents", "--full"}
	case 2:
		return []string{"supervisor", "status"}
	case 3:
		return []string{"note", "list"}
	default:
		return []string{"--as", fmt.Sprintf("perf worker %d", i), "log", "add", fmt.Sprintf("perf concurrency line %d", i)}
	}
}

func TestConcurrentCalls(t *testing.T) {
	requirePerf(t)
	d := newDesk(t, defaultDesk())
	probe := probeLock(d.stateDir)
	started := time.Now()
	samples := make([]sample, concurrentCalls)
	var wg sync.WaitGroup
	for i := range concurrentCalls {
		wg.Go(func() { samples[i] = d.run(t.Context(), concurrentArgs(i)...) })
	}
	wg.Wait()
	all := time.Since(started)
	hold := probe.stop()
	var slowest time.Duration
	for i, s := range samples {
		if s.Exit != 0 {
			t.Errorf("call %d %v: exit %d:\n%s", i, concurrentArgs(i), s.Exit, s.Output)
		}
		slowest = max(slowest, s.Wall)
	}
	rep.check(t, fmt.Sprintf("%d calls", concurrentCalls), "wall", all.Seconds(), concurrentAll)
	rep.check(t, fmt.Sprintf("%d calls", concurrentCalls), "hold", hold.Seconds(), concurrentHold)
	rep.check(t, "slowest call", "wall", slowest.Seconds(), measured)
}
