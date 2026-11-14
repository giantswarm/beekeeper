package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// RoutineLine is deliberately an allowlist: new and unknown events wake
// immediately. In particular, faults, pages, thresholds, leases and reports
// must not inherit the policy of a routine line emitted by the same source.
func (w *Watcher) RoutineLine(line string) bool {
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
		return len(fields) >= 6 && w.Host.cfg.Alerts.Team != "" &&
			strings.EqualFold(fields[3], "notify") &&
			!strings.EqualFold(fields[4], w.Host.cfg.Alerts.Team)
	}
	return false
}

func (w *Watcher) OutputEvent(line string) {
	stamped := w.Clk().Now().Format("15:04:05") + " " + line
	routine := w.BatchEvery > 0 && w.RoutineLine(line)
	w.Mu.Lock()
	defer w.Mu.Unlock()
	if routine {
		w.batch = append(w.batch, stamped)
		return
	}
	_, _ = fmt.Fprintln(w.Host.out, stamped)
}

// FlushBatch makes one physical stdout line: newline-delimited monitors
// would wake once per item if the batch contained literal line breaks.
// JSON retains each item's timestamp and embedded newlines losslessly.
func (w *Watcher) FlushBatch() {
	w.Mu.Lock()
	defer w.Mu.Unlock()
	if len(w.batch) == 0 {
		return
	}
	items, _ := json.Marshal(w.batch) // strings always marshal
	_, _ = fmt.Fprintf(w.Host.out, "%s BATCH: %s\n", w.Clk().Now().Format("15:04:05"), items)
	w.batch = nil
}

// A dedicated clock keeps a slow poll from delaying the routine tick. No
// empty notices are printed. Action output does not flush or reset it.
func (w *Watcher) BatchLoop(ctx context.Context) {
	for {
		fire, stop := w.Clk().Timer(w.BatchEvery)
		select {
		case <-ctx.Done():
			stop()
			return // every buffered item already has its durable watch event
		case <-fire:
			stop()
			w.FlushBatch()
		}
	}
}
