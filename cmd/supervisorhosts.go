package cmd

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// hostDrainRetry paces the next update of a host that answered busy while
// the coordinator recorded no execution on it.
const hostDrainRetry = 10 * time.Minute

// updateHosts brings every execution host (agents.hosts) to the release
// this coordinator runs, with agents hosts update, once the coordinator
// runs a newer one than the host's pin: a host on which executions run
// drains first, a held host is tried again hourly, and a development build
// updates none. Nothing is updated while beekeeper stop stands. Each new
// hold is handed on once as the loop's finding.
func (l *supervisorLoop) updateHosts(ctx context.Context, st *state.State) {
	a := l.a
	version := coordinatorVersion()
	if !releaseVersion(version) || st.Stop != nil || len(a.cfg.Agents.Hosts) == 0 {
		return
	}
	for _, selector := range slices.Sorted(maps.Keys(a.cfg.Agents.Hosts)) {
		pin := st.HostPin(selector)
		if pin != nil && pin.Identity != a.cfg.Agents.Hosts[selector].Identity {
			pin = nil
		}
		switch {
		case pin != nil && pin.Held == "" && pin.Version == version:
			continue
		case pin != nil && pin.Held != "" && pin.HeldVersion == version && a.now.Sub(pin.Tried) < hostUpdateRetry:
			continue
		case pin != nil && pin.Held == "" && a.now.Sub(pin.Tried) < hostDrainRetry:
			continue
		}
		if n := hostExecutions(st, selector); n > 0 {
			l.sayOnce("host drains "+selector+" "+version, "SUPERVISOR HOST DRAINS %s: %d executions run on it; it updates to %s once they end", selector, n, version)
			continue
		}
		out, err := l.run(ctx, agentsName, "hosts", hostUpdateCmdName, selector)
		if err != nil {
			l.sayOnce("host update "+selector+" "+version, "SUPERVISOR HOST WAITS %s: %v", selector, err)
			continue
		}
		_, _ = fmt.Fprintln(a.out, "SUPERVISOR HOST UPDATED "+oneLine(out))
	}
	if err := l.reportHeldHosts(); err != nil {
		l.sayOnce("host held report", "SUPERVISOR HOST HELD not reported: %v", err)
	}
}

// reportHeldHosts hands each hold not yet reported on as one finding
// (supervisor.file, which guide watch delivers as GUIDE PROBLEM) and says
// it, once per hold.
func (l *supervisorLoop) reportHeldHosts() error {
	st, err := l.a.store.Peek()
	if err != nil || !slices.ContainsFunc(st.HostPins, func(p state.HostPin) bool { return p.Held != "" && !p.Reported }) {
		return err
	}
	var lines []string
	err = l.a.store.Update(func(st *state.State) ([]state.Event, error) {
		lines = nil
		var events []state.Event
		for i := range st.HostPins {
			p := &st.HostPins[i]
			if p.Held == "" || p.Reported {
				continue
			}
			p.Reported = true
			finding := fmt.Sprintf("Execution host %s could not be updated to %s and takes no start: %s. The other execution hosts keep running; beekeeper agents hosts update %s lifts the hold once its cause is fixed, and the supervisor loop tries it again hourly.", p.Selector, p.HeldVersion, p.Held, p.Selector)
			at := p.HeldAt.UTC().Format(time.RFC3339)
			events = append(events, event(loopParty, "supervisor."+superloop.KindFile, "[host-held %s %s] execution host %s held at %s%s%s", p.Selector, at, p.Selector, at, fileReportSeparator, finding))
			lines = append(lines, "SUPERVISOR HOST HELD "+finding)
		}
		return events, nil
	})
	for _, line := range lines {
		_, _ = fmt.Fprintln(l.a.out, line)
	}
	return err
}
