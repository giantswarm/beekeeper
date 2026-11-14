package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// routineLine is deliberately an allowlist: new and unknown events wake
// immediately. In particular, faults, pages, thresholds, leases and reports
// must not inherit the policy of a routine line emitted by the same source.
func (w *watcher) routineLine(line string) bool {
	switch {
	case strings.HasPrefix(line, "SESSIONS "), strings.HasPrefix(line, "CENTRAL agents.register "):
		return true
	// These unresolved failures share the doctor source with its chores.
	// In particular, a failed reset uses lowercase "failed"; a successful
	// reset also names "failed units", so matching that word alone is wrong.
	case strings.HasPrefix(line, "DOCTOR FAULT"), strings.HasPrefix(line, "DOCTOR stale binary:"),
		strings.HasPrefix(line, "DOCTOR the failed units "):
		return false
	case strings.HasPrefix(line, "DOCTOR "):
		return !strings.Contains(line, "FAILED")
	case strings.HasPrefix(line, "ENDED HIGH LOAD"), strings.HasPrefix(line, "ENDED LOAD RISING"),
		strings.HasPrefix(line, "ENDED CPU PRESSURE"), strings.HasPrefix(line, "ENDED MEMORY PRESSURE"),
		strings.HasPrefix(line, "ENDED READS SLOWED"):
		return true
	case strings.HasPrefix(line, "ALERT "):
		fields := strings.Fields(line)
		// ALERT <kind> <installation> <severity> <team> ...; our team's
		// marker is uppercase. No configured team means no known other team.
		return len(fields) >= 6 && w.a.cfg.Alerts.Team != "" &&
			strings.EqualFold(fields[3], "notify") &&
			!strings.EqualFold(fields[4], w.a.cfg.Alerts.Team)
	}
	return false
}

func (w *watcher) outputEvent(line string) {
	stamped := w.clk().Now().Format("15:04:05") + " " + line
	routine := w.batchEvery > 0 && w.routineLine(line)
	w.mu.Lock()
	defer w.mu.Unlock()
	if routine {
		w.batch = append(w.batch, stamped)
		return
	}
	_, _ = fmt.Fprintln(w.a.out, stamped)
}

// flushBatch makes one physical stdout line: newline-delimited monitors
// would wake once per item if the batch contained literal line breaks.
// JSON retains each item's timestamp and embedded newlines losslessly.
func (w *watcher) flushBatch() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.batch) == 0 {
		return
	}
	items, _ := json.Marshal(w.batch) // strings always marshal
	_, _ = fmt.Fprintf(w.a.out, "%s BATCH: %s\n", w.clk().Now().Format("15:04:05"), items)
	w.batch = nil
}

// A dedicated clock keeps a slow poll from delaying the routine tick. No
// empty notices are printed. Action output does not flush or reset it.
func (w *watcher) batchLoop(ctx context.Context) {
	for {
		fire, stop := w.clk().Timer(w.batchEvery)
		select {
		case <-ctx.Done():
			stop()
			return // every buffered item already has its durable watch event
		case <-fire:
			stop()
			w.flushBatch()
		}
	}
}
