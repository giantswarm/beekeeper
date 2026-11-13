package loop

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
	"github.com/giantswarm/beekeeper/internal/update"
)

// hostDrainRetry paces the next update of a host that answered busy while
// the coordinator recorded no execution on it.
const hostDrainRetry = 10 * time.Minute

// HostBehindBound is how long an execution host may stay behind the
// coordinator's release, draining or waiting, before the supervisor loop
// hands it on as a fault.
const HostBehindBound = time.Hour

// HostDrains says the execution host pinned by pin drains for the
// coordinator's release version: it is pinned to another release, so it
// takes no new start until its update succeeds, and the executions on it
// reach the turn boundary the update waits for. A host never pinned to a
// release is not behind one, and a host whose update waits on the
// coordinator's failed lookup of version keeps taking starts until that
// lookup is due again. The caller checks that the coordinator runs a
// release that is ready for its hosts (CoordinatorHostUpdateReady).
func HostDrains(pin state.HostPin, version string, b *state.Budget, now time.Time) bool {
	if pin.Version == "" || pin.Version == version {
		return false
	}
	return pin.UpdateError == "" || pin.UpdateVersion != version || !HostHoldWait(state.HostPin{Held: pin.UpdateError, Tried: pin.Tried}, b, now)
}

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
// runs a newer one than the host's pin or the host's entry names a policy
// digest the pin does not answer (hostPinPolicyCurrent): a host on which
// executions are in a turn drains first (a parked worker's execution between
// turns does not hold it back), taking no new start (HostDrains), so it
// updates at the first pass that finds them between turns; a held host is
// tried again hourly, and a development build updates none. Nothing is
// updated while beekeeper stop stands. Each new hold is handed on once as the
// loop's finding, and so is a host that stays behind for more than one
// release or for longer than HostBehindBound, with why.
func (l *Loop) UpdateHosts(ctx context.Context, st *state.State) {
	a := l.Host
	version := Cmd.CoordinatorVersion()
	if !Cmd.ReleaseVersion(version) || st.Stop != nil || len(a.Cfg().Agents.Hosts) == 0 {
		return
	}
	if err := Cmd.CoordinatorHostUpdateReady(version); err != nil {
		l.SayOnce("host probation "+version, "SUPERVISOR HOST WAITS: %v", err)
		return
	}
	for _, selector := range slices.Sorted(maps.Keys(a.Cfg().Agents.Hosts)) {
		if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
			return
		}
		l.LifecyclePhase(loopPhaseTidy)
		h := a.Cfg().Agents.Hosts[selector]
		pin := st.HostPin(selector)
		if pin != nil && pin.Identity != h.Identity {
			pin = nil
		}
		if pin != nil && pin.UpdateError != "" && pin.UpdateVersion == version && HostHoldWait(state.HostPin{Held: pin.UpdateError, Tried: pin.Tried}, st.Budget, a.TimeNow()) {
			continue
		}
		switch {
		case pin != nil && pin.Held == "" && pin.Version == version && a.HostPinToolsCurrent(*pin, h) && hostPinPolicyCurrent(*pin, h):
			continue
		case pin != nil && pin.Held != "" && pin.HeldVersion == version && HostHoldWait(*pin, st.Budget, a.TimeNow()):
			continue
		case pin != nil && pin.Held == "" && a.TimeNow().Sub(pin.Tried) < hostDrainRetry:
			continue
		}
		if n := Cmd.HostExecutions(st, selector); n > 0 {
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
				l.noteBehind(selector, version, Cmd.OneLine(err.Error()))
			}
			continue
		}
		_, _ = fmt.Fprintln(a.Out(), "SUPERVISOR HOST UPDATED "+Cmd.OneLine(out))
	}
	if err := l.reportHeldHosts(); err != nil {
		l.SayOnce("host held report", "SUPERVISOR HOST HELD not reported: %v", err)
	}
}

// hostPinPolicyCurrent says whether pin answers the policy digest the host's
// agents.hosts entry h names: none, the pinned one, or the one it named when
// the host was last pinned. Any other is an operator's change of the host's
// policy, which makes the host due for an update as a new release does: the
// update prepares the host's policy again and pins its digest. A digest the
// host does not prepare, such as a stale deprecated one, is updated for once.
func hostPinPolicyCurrent(pin state.HostPin, h execution.Host) bool {
	return h.PolicyDigest == "" || h.PolicyDigest == pin.PolicyDigest || h.PolicyDigest == pin.Configured
}

// drainingWorkerNames names the workers whose executions hold the host
// selector's update back (hostExecutions).
func drainingWorkerNames(st *state.State, selector string) []string {
	var names []string
	for i := range st.Starts {
		if x := st.Starts[i].Execution; x != nil && x.Selector == selector && !x.Terminal() && !Cmd.ParkedBetweenTurns(st, x) {
			names = append(names, st.Starts[i].Name)
		}
	}
	return names
}

// noteBehind records that the host selector did not follow the
// coordinator's release version (drains or waits, for reason), and hands it
// on once as the loop's finding when the host stays behind past the first
// release it missed or for longer than HostBehindBound. A held host is
// handed on by its hold instead.
func (l *Loop) noteBehind(selector, version, reason string) {
	a := l.Host
	now := a.TimeNow()
	var finding string
	err := a.Store().Update(func(st *state.State) ([]state.Event, error) {
		finding = ""
		pin := st.HostPin(selector)
		if pin == nil {
			st.HostPins = append(st.HostPins, state.HostPin{Selector: selector, Identity: a.Cfg().Agents.Hosts[selector].Identity})
			pin = &st.HostPins[len(st.HostPins)-1]
		}
		switch {
		case pin.Version == version || pin.Held != "":
			return nil, nil
		case pin.Behind == "" || pin.BehindSince.IsZero():
			// A pin recorded behind before BehindSince was kept starts
			// its bound now.
			pin.Behind, pin.BehindSince = cmp.Or(pin.Behind, version), now
			return nil, nil
		case pin.BehindReported || pin.Behind == version && now.Sub(pin.BehindSince) < HostBehindBound:
			return nil, nil
		}
		pin.BehindReported = true
		on := pin.Version
		if on == "" {
			on = "a release it was never pinned to"
		}
		missed := pin.Behind
		if pin.Behind != version {
			missed += " and does not follow " + version + " either"
		}
		if pin.Version != "" {
			missed += ", and takes no new start until it updates"
		}
		finding = fmt.Sprintf("Execution host %s stays on %s, behind the coordinator for %s: it did not follow %s: %s. beekeeper agents hosts update %s retries it at once.", selector, on, Cmd.ShortDur(now.Sub(pin.BehindSince).Round(time.Minute)), missed, reason, selector)
		return []state.Event{Cmd.Event(LoopParty, "supervisor."+superloop.KindFile, "[host-behind %s %s] execution host %s behind since %s%s%s", selector, version, selector, pin.Behind, FileReportSeparator, finding)}, nil
	})
	if err != nil {
		l.SayOnce("host behind report "+selector, "SUPERVISOR HOST BEHIND not reported: %v", err)
		return
	}
	if finding != "" {
		_, _ = fmt.Fprintln(a.Out(), "SUPERVISOR HOST BEHIND "+finding)
	}
}

// reportHeldHosts hands each hold not yet reported on as one finding
// (supervisor.file, which guide watch delivers as GUIDE PROBLEM) and says
// it, once per hold.
func (l *Loop) reportHeldHosts() error {
	st, err := l.Host.Store().Peek()
	if err != nil || !slices.ContainsFunc(st.HostPins, func(p state.HostPin) bool { return HostHoldBlocks(p) && !p.Reported }) {
		return err
	}
	var lines []string
	err = l.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
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
			events = append(events, Cmd.Event(LoopParty, "supervisor."+superloop.KindFile, "[host-held %s %s] execution host %s held at %s%s%s", p.Selector, at, p.Selector, at, FileReportSeparator, finding))
			lines = append(lines, "SUPERVISOR HOST HELD "+finding)
		}
		return events, nil
	})
	for _, line := range lines {
		_, _ = fmt.Fprintln(l.Host.Out(), line)
	}
	return err
}
