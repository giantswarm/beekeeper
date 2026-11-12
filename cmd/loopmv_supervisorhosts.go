package cmd

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
	"github.com/giantswarm/beekeeper/internal/update"
)

// hostDrainRetry paces the next update of a host that answered busy while
// the coordinator recorded no execution on it.
const hostDrainRetry = 10 * time.Minute

// Legacy lookup and probation holds never changed the installed host. Actual
// installation, preparation and policy failures remain start-blocking.
func HostHoldBlocks(pin state.HostPin) bool {
	if pin.Held == "" || strings.Contains(pin.Held, update.ErrProbationWatched.Error()) {
		return false
	}
	return !strings.HasPrefix(pin.Held, "looking up release ") && !github.RateLimited(errors.New(pin.Held))
}

func HostHoldWait(pin state.HostPin, b *state.Budget, now time.Time) bool {
	if strings.Contains(pin.Held, update.ErrProbationWatched.Error()) {
		return false // Retry legacy probation holds as soon as the coordinator settles.
	}
	if reset, ok := hostHoldRateReset(pin.Held); ok {
		return now.Before(reset)
	}
	if github.RateLimited(errors.New(pin.Held)) && b != nil && b.Remaining > 0 && now.After(pin.Tried) && !b.At.Before(pin.Tried) {
		return false
	}
	return now.Sub(pin.Tried) < HostUpdateRetry
}

func hostHoldRateReset(held string) (time.Time, bool) {
	_, rest, ok := strings.Cut(held, "resets at ")
	if !ok {
		return time.Time{}, false
	}
	token := strings.Fields(rest)
	if len(token) == 0 {
		return time.Time{}, false
	}
	raw := strings.TrimRight(token[0], ".,;:)")
	t, err := time.Parse(time.RFC3339, raw)
	return t, err == nil
}

// UpdateHosts brings every execution host (agents.hosts) to the release
// this coordinator runs, with agents hosts update, once the coordinator
// runs a newer one than the host's pin: a host on which executions are in a
// turn drains first (a parked worker's execution between turns does not hold
// it back), a held host is tried again hourly, and a development build
// updates none. Nothing is updated while beekeeper stop stands. Each new
// hold is handed on once as the loop's finding, and so is a host that stays
// behind for more than one release, with why.
func (l *Loop) UpdateHosts(ctx context.Context, st *state.State) {
	a := l.Host
	version := coordinatorVersion()
	if !releaseVersion(version) || st.Stop != nil || len(a.cfg.Agents.Hosts) == 0 {
		return
	}
	if err := coordinatorHostUpdateReady(version); err != nil {
		l.SayOnce("host probation "+version, "SUPERVISOR HOST WAITS: %v", err)
		return
	}
	for _, selector := range slices.Sorted(maps.Keys(a.cfg.Agents.Hosts)) {
		if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
			return
		}
		l.LifecyclePhase(loopPhaseTidy)
		pin := st.HostPin(selector)
		if pin != nil && pin.Identity != a.cfg.Agents.Hosts[selector].Identity {
			pin = nil
		}
		if pin != nil && pin.UpdateError != "" && pin.UpdateVersion == version && HostHoldWait(state.HostPin{Held: pin.UpdateError, Tried: pin.Tried}, st.Budget, a.timeNow()) {
			continue
		}
		switch {
		case pin != nil && pin.Held == "" && pin.Version == version && a.hostPinToolsCurrent(*pin, a.cfg.Agents.Hosts[selector]):
			continue
		case pin != nil && pin.Held != "" && pin.HeldVersion == version && HostHoldWait(*pin, st.Budget, a.timeNow()):
			continue
		case pin != nil && pin.Held == "" && a.timeNow().Sub(pin.Tried) < hostDrainRetry:
			continue
		}
		if n := hostExecutions(st, selector); n > 0 {
			reason := fmt.Sprintf("%d executions are in a turn on it (%s); it updates to %s once they are between turns", n, strings.Join(drainingWorkerNames(st, selector), ", "), version)
			l.SayOnce("host drains "+selector+" "+version, "SUPERVISOR HOST DRAINS %s: %s", selector, reason)
			l.noteBehind(selector, version, reason)
			continue
		}
		out, err := l.Run(ctx, agentsName, "hosts", hostUpdateCmdName, selector)
		if err != nil {
			if errors.Is(err, state.ErrRecoveryWriterFenced) {
				return
			}
			l.SayOnce("host update "+selector+" "+version, "SUPERVISOR HOST WAITS %s: %v", selector, err)
			if !strings.Contains(err.Error(), hostUpdateDeferred) {
				l.noteBehind(selector, version, oneLine(err.Error()))
			}
			continue
		}
		_, _ = fmt.Fprintln(a.out, "SUPERVISOR HOST UPDATED "+oneLine(out))
	}
	if err := l.reportHeldHosts(); err != nil {
		l.SayOnce("host held report", "SUPERVISOR HOST HELD not reported: %v", err)
	}
}

// drainingWorkerNames names the workers whose executions hold the host
// selector's update back (hostExecutions).
func drainingWorkerNames(st *state.State, selector string) []string {
	var names []string
	for i := range st.Starts {
		if x := st.Starts[i].Execution; x != nil && x.Selector == selector && !x.Terminal() && !parkedBetweenTurns(st, x) {
			names = append(names, st.Starts[i].Name)
		}
	}
	return names
}

// noteBehind records that the host selector did not follow the
// coordinator's release version (drains or waits, for reason), and hands it
// on once as the loop's finding when the host stays behind past the first
// release it missed. A held host is handed on by its hold instead.
func (l *Loop) noteBehind(selector, version, reason string) {
	a := l.Host
	var finding string
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		finding = ""
		pin := st.HostPin(selector)
		if pin == nil {
			st.HostPins = append(st.HostPins, state.HostPin{Selector: selector, Identity: a.cfg.Agents.Hosts[selector].Identity})
			pin = &st.HostPins[len(st.HostPins)-1]
		}
		switch {
		case pin.Version == version || pin.Held != "":
			return nil, nil
		case pin.Behind == "":
			pin.Behind = version
			return nil, nil
		case pin.Behind == version || pin.BehindReported:
			return nil, nil
		}
		pin.BehindReported = true
		on := pin.Version
		if on == "" {
			on = "a release it was never pinned to"
		}
		finding = fmt.Sprintf("Execution host %s stays on %s for more than one release: it did not follow %s and does not follow %s either, its current policy remains subject to admission checks: %s. beekeeper agents hosts update %s retries it at once.", selector, on, pin.Behind, version, reason, selector)
		return []state.Event{event(LoopParty, "supervisor."+superloop.KindFile, "[host-behind %s %s] execution host %s behind since %s%s%s", selector, version, selector, pin.Behind, FileReportSeparator, finding)}, nil
	})
	if err != nil {
		l.SayOnce("host behind report "+selector, "SUPERVISOR HOST BEHIND not reported: %v", err)
		return
	}
	if finding != "" {
		_, _ = fmt.Fprintln(a.out, "SUPERVISOR HOST BEHIND "+finding)
	}
}

// reportHeldHosts hands each hold not yet reported on as one finding
// (supervisor.file, which guide watch delivers as GUIDE PROBLEM) and says
// it, once per hold.
func (l *Loop) reportHeldHosts() error {
	st, err := l.Host.store.Peek()
	if err != nil || !slices.ContainsFunc(st.HostPins, func(p state.HostPin) bool { return HostHoldBlocks(p) && !p.Reported }) {
		return err
	}
	var lines []string
	err = l.Host.store.Update(func(st *state.State) ([]state.Event, error) {
		lines = nil
		var events []state.Event
		for i := range st.HostPins {
			p := &st.HostPins[i]
			if !HostHoldBlocks(*p) || p.Reported {
				continue
			}
			p.Reported = true
			finding := fmt.Sprintf("Execution host %s could not be updated to %s and takes no start: %s. The other execution hosts keep running; beekeeper agents hosts update %s lifts the hold once its cause is fixed, and the supervisor loop tries it again hourly.", p.Selector, p.HeldVersion, p.Held, p.Selector)
			at := p.HeldAt.UTC().Format(time.RFC3339)
			events = append(events, event(LoopParty, "supervisor."+superloop.KindFile, "[host-held %s %s] execution host %s held at %s%s%s", p.Selector, at, p.Selector, at, FileReportSeparator, finding))
			lines = append(lines, "SUPERVISOR HOST HELD "+finding)
		}
		return events, nil
	})
	for _, line := range lines {
		_, _ = fmt.Fprintln(l.Host.out, line)
	}
	return err
}
