package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// standbyUnit is the standby watch's installed unit (beekeeper install).
const standbyUnit = "beekeeper-notify.service"

// unitActive is the ActiveState of a running unit.
const unitActive = "active"

// stopHolds are the holds a stop sets: every merge and every GitHub call.
var stopHolds = []string{holdMerges, holdGitHub}

// turnPatterns match the units of beekeeper's starts, wakes, reopens and
// status reports.
var turnPatterns = []string{"beekeeper-agent-*", "beekeeper-agy-*", "beekeeper-codex-*", "beekeeper-omp-*", "beekeeper-wake-*", reopenUnitPrefix + "*", "beekeeper-report-*"}

// stopHoldReason is the reason of a hold the stop sets.
func stopHoldReason(reason string) string { return "beekeeper stop: " + reason }

// endStopped ends a session the stop stops (endSession); a seam for the
// tests.
var endStopped = endSession

// stopText says the standing stop s.
func (a *app) stopText(s *state.Stop) string {
	return fmt.Sprintf("beekeeper is stopped by %s since %s: %s (beekeeper resume lifts it)", s.By.Name, clock(a.now, s.At), s.Reason)
}

// refuseStopped is the refusal of what while st's stop stands, nil without
// one.
func (a *app) refuseStopped(st *state.State, what string) error {
	if st.Stop == nil {
		return nil
	}
	return refused("%s refused: %s", what, a.stopText(st.Stop))
}

// readStop is the standing stop, nil without one or when the state cannot
// be read (the caller's own read reports that).
func (a *app) readStop() *state.Stop {
	st, err := a.store.Read()
	if err != nil {
		return nil
	}
	return st.Stop
}

func (a *app) stopCmd() *cobra.Command {
	var reason string
	c := &cobra.Command{
		Use:   "stop --reason <words>",
		Short: "Stop every start, wake, revive, import and merge, and end the workers' sessions, until beekeeper resume",
		Long: `Stop the machine's sessions at once, as the person says stop, and keep them
stopped until beekeeper resume:

- records the stop, with its reason and who stopped;
- holds "merges" and "github" (beekeeper hold), unless a hold that holds
  them whole stands already: one that lets a merge through (--except, a
  tool-release window) or ends by itself (--until, --lift-when) gives way
  to the stop's, and resume puts it back as it was;
- ends the devctl of every running gated merge and promotion before it
  merges, saying whether GitHub reports it merged before the stop (a
  release wait) or nothing merged; a waiting merge keeps its place;
- stops every unit of beekeeper's starts, wakes, reopens and status
  reports (beekeeper-agent-*, beekeeper-wake-*, beekeeper-reopen-*,
  beekeeper-report-*), the standby service (beekeeper-notify.service)
  and the units stop.units names (a timer of the desk's own that would
  start or change something);
- ends the turn of every session beekeeper started and stops it: its
  headless turn, its CLI and what it left running, a background session
  through claude stop so its daemon does not resume it.

It never touches a session the person started, the guide's or the
supervisor's session, or the session that runs it.

While the stop stands, agents start (--force included), agents handover,
agents wake, agents desktop, a timer's wake, a note answer's wake, a
parked agent's resume and the gate's wakes are refused (exit 3, the
stop's reason named); a role's successor, a revive, a reopen, a desktop
import and a status report do not start; a wake timer that falls due
waits and fires after the resume; the watch's doctor reopens nothing.
beekeeper status, capacity and the watch say the stop with its reason.

Run it again while stopped to stop what has come up since: the record
keeps its first reason. It prints what it stopped. A unit that does not
stop, a session that does not end or a devctl that cannot be signalled is
named on a FAILED line and makes it exit 1; the stop stands.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(reason) == "" {
				return usageErr("--reason is required: the person's words")
			}
			return a.stop(cmd.Context(), strings.TrimSpace(reason))
		},
	}
	c.Flags().StringVarP(&reason, "reason", "r", "", "why: the person's words (required)")
	return c
}

func (a *app) resumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Lift beekeeper stop: its holds, and the units it stopped back",
		Long: `Lift the standing beekeeper stop: the installed units it stopped (the
standby service, stop.units) start again, the holds the stop set are
lifted, a hold that stood before it stays, one it replaced is put back as
it was (unless its time passed meanwhile), and starts, wakes, revives,
reopens and imports run again. A unit that does not start again is named
on a FAILED line and makes it exit 1: beekeeper stays stopped, with that
unit still to start, and resume runs again once it is fixed. The capacity is what it was: the stop never
changes capacity.floor or capacity.ceiling. A wake timer that fell due
during the stop fires at the watch's next poll.

It starts no session: it names the sessions the stop ended, which wait for
a wake (beekeeper agents wake), and touches no session the person started.
It prints what it lifted and restarted.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.resume(cmd.Context()) },
	}
}

// stop records the stop and its holds, then ends the running merges' devctl,
// stops the units and ends the sessions it stops, and records those. Any of
// them that fails is an error once the rest is done; the stop stands.
func (a *app) stop(ctx context.Context, reason string) error {
	me, err := a.caller()
	if err != nil {
		return err
	}
	var rec state.Stop
	var again bool
	var held, replaced []string
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		if again = st.Stop != nil; !again {
			st.Stop = &state.Stop{Reason: reason, By: me, At: a.now.UTC()}
		}
		held, replaced = nil, nil
		for _, t := range stopHolds {
			i := slices.IndexFunc(st.Holds, func(h state.Hold) bool { return h.Target == t && h.Active(a.now) })
			if i >= 0 && fullHold(st.Holds[i]) {
				continue // it holds as long as the stop's would, the first run's too
			}
			if i >= 0 {
				st.Stop.Replaced = append(st.Stop.Replaced, st.Holds[i])
				replaced = append(replaced, holdTarget(st.Holds[i]))
			}
			st.Holds = slices.DeleteFunc(st.Holds, func(h state.Hold) bool { return h.Target == t || h.Expired(a.now) })
			st.Holds = append(st.Holds, state.Hold{Target: t, Reason: stopHoldReason(st.Stop.Reason), By: me, At: a.now.UTC()})
			if !slices.Contains(st.Stop.Holds, t) {
				st.Stop.Holds = append(st.Stop.Holds, t)
			}
			held = append(held, t)
		}
		rec = *st.Stop
		if again {
			return []state.Event{event(me, "stop.again", "%s", reason)}, nil
		}
		return []state.Event{event(me, "stop", "%s", reason)}, nil
	})
	if err != nil {
		return err
	}
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
	if again {
		p("stop: %s; stopping what came up since", a.stopText(&rec))
	} else {
		p("stop: recorded, by %s: %s", me.Name, reason)
	}
	for _, h := range replaced {
		p("stop: the hold on %s gives way to the stop's; resume puts it back", h)
	}
	for _, t := range held {
		p("stop: held %s", t)
	}
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	spare := spared(st, me)
	var failed []string
	lines, f := a.endMerges(st, me)
	failed = append(failed, f...)
	units, ul, f := a.stopUnits(ctx, spare)
	lines, failed = append(lines, ul...), append(failed, f...)
	ended, el, f := a.endWorkers(ctx, st, spare)
	lines, failed = append(lines, el...), append(failed, f...)
	for _, l := range lines {
		p("stop: %s", l)
	}
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		if st.Stop == nil {
			return nil, nil // a resume came between
		}
		for _, u := range units {
			if !slices.Contains(st.Stop.Units, u) {
				st.Stop.Units = append(st.Stop.Units, u)
			}
		}
		for _, n := range ended {
			if !slices.Contains(st.Stop.Sessions, n) {
				st.Stop.Sessions = append(st.Stop.Sessions, n)
			}
		}
		evs := []state.Event{event(me, "stop.stopped", "units %s; sessions %s", listOrNone(units), listOrNone(ended))}
		if len(failed) > 0 {
			evs = append(evs, event(me, "stop.failed", "%s", strings.Join(failed, "; ")))
		}
		return evs, nil
	})
	if err != nil {
		return err
	}
	if len(failed) > 0 {
		for _, l := range failed {
			p("stop: FAILED %s", l)
		}
		return fmt.Errorf("the stop stands, but %d of its steps failed (above); act on them and run beekeeper stop again", len(failed))
	}
	p("stop: nothing starts, wakes, revives, imports or merges until beekeeper resume")
	return nil
}

// fullHold reports whether h holds its target whole until someone lifts
// it: no exception, no tool-release window, no end of its own (a time, a
// probe) that would lift it while the stop stands.
func fullHold(h state.Hold) bool {
	return h.Except == "" && h.ExceptSession == "" && h.Tool == "" && h.Until.IsZero() && h.LiftWhen == ""
}

// endMerges ends the devctl of every running gated merge or promotion
// before it merges: the stop's merges hold refuses new ones, but a devctl
// already waiting for CI would merge on. It is a TaskStop's stopMerges on
// the running merges only (a waiting merge keeps its place behind the
// hold): only a merge's recorded merge-child is signalled, and each line
// says, as GitHub reports it, whether the pull request merged before the
// stop or nothing merged. A devctl that still runs after the stop's wait
// fails.
func (a *app) endMerges(st *state.State, me state.Party) (lines, failed []string) {
	running := func(m state.Merge) bool { return m.Phase == state.Running }
	var children []state.Merge
	for _, m := range st.Merges {
		if running(m) && m.Child != 0 && proc.Alive(m.Child) {
			children = append(children, m)
		}
	}
	if len(children) == 0 {
		return nil, nil
	}
	said, err := stopMerges(a.store, me, running, true, func(state.Merge) string { return "beekeeper stop" })
	if err != nil {
		return nil, []string{fmt.Sprintf("ending the running merges: %v", err)}
	}
	for _, m := range children {
		if proc.Alive(m.Child) {
			failed = append(failed, fmt.Sprintf("the devctl of %s (merge-child pid %d) still runs", m.Key(), m.Child))
		}
	}
	return said, failed
}

// spared are the sessions a stop leaves running: the supervisor's, the
// guide's and the caller's own. A session the person started is spared
// by not being one of beekeeper's starts.
func spared(st *state.State, me state.Party) []state.Party {
	out := []state.Party{me}
	if st.Supervisor != nil {
		out = append(out, st.Supervisor.Party)
	}
	if st.Guide != nil && st.Guide.Holder != nil {
		out = append(out, st.Guide.Holder.Party)
	}
	return out
}

// sparedUnit reports whether unit is a start's, wake's or reopen's unit of
// one of the spared sessions, or the unit this process runs in.
func sparedUnit(unit, own string, spare []state.Party) bool {
	if unit == own {
		return true
	}
	return slices.ContainsFunc(spare, func(p state.Party) bool {
		if _, hid, ok := harnessForHost(p.HostSession); ok && hid != "" {
			if strings.HasPrefix(unit, agyUnit(hid)) || strings.HasPrefix(unit, codexUnit(hid)) || strings.HasPrefix(unit, ompUnit(hid)) {
				return true
			}
		}
		id := p.Session
		if id == "" {
			return false
		}
		return strings.HasPrefix(unit, "beekeeper-agent-"+id[:min(8, len(id))]) ||
			strings.HasPrefix(unit, wakePrefix(id)) || strings.HasPrefix(unit, reopenPrefix(id))
	})
}

// stopUnits stops the running units of beekeeper's starts, wakes and
// reopens but the spared sessions', and the installed units that run of
// the standby service and stop.units. It returns the installed units it
// stopped, which resume starts again, a line for each unit stopped and one
// for each that did not stop.
func (a *app) stopUnits(ctx context.Context, spare []state.Party) (installed, lines, failed []string) {
	if !plat.Launcher.Available() {
		return nil, []string{"no unit stopped: this machine has no unit launcher (systemd --user); the standby watch honours the stop"}, nil
	}
	own := ownUnit()
	for _, u := range plat.Launcher.Running(ctx, true, turnPatterns...) {
		if sparedUnit(u, own, spare) {
			continue
		}
		if err := plat.Launcher.Stop(ctx, u); err != nil {
			failed = append(failed, fmt.Sprintf("stopping %s: %v", u, err))
			continue
		}
		lines = append(lines, "stopped "+u)
	}
	for _, u := range append([]string{standbyUnit}, a.cfg.Stop.Units...) {
		switch plat.Launcher.State(ctx, u) {
		case unitActive, "activating", "reloading":
		default:
			continue
		}
		if err := plat.Launcher.Stop(ctx, u); err != nil {
			failed = append(failed, fmt.Sprintf("stopping %s: %v", u, err))
			continue
		}
		installed = append(installed, u)
		lines = append(lines, "stopped "+u+"; resume starts it again")
	}
	return installed, lines, failed
}

// endWorkers ends every live session beekeeper started but the spared
// ones, in parallel, and returns the names it ended, a line for each and
// one for each it could not end.
func (a *app) endWorkers(ctx context.Context, st *state.State, spare []state.Party) (ended, lines, failed []string) {
	sessions, _, err := a.sessions()
	if err != nil {
		return nil, nil, []string{fmt.Sprintf("no session ended: reading the sessions failed: %v", err)}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, s := range sessions {
		p := state.Party{Session: s.ID, HostSession: s.HostID, Name: s.Name}
		if !started(st, p) || slices.ContainsFunc(spare, p.Is) {
			continue
		}
		wg.Go(func() {
			n, err := endStopped(ctx, s)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed = append(failed, fmt.Sprintf("ending %q (session %s): %v", s.Name, s.ID, err))
				return
			}
			ended = append(ended, s.Name)
			lines = append(lines, endedLine(s, n))
		})
	}
	wg.Wait()
	slices.Sort(ended)
	slices.Sort(lines)
	slices.Sort(failed)
	return ended, lines, failed
}

// endedLine says the session s the stop ended with n processes.
func endedLine(s *claude.Session, n int) string {
	how := ""
	if s.Background {
		how = " through claude stop"
	}
	return fmt.Sprintf("ended %q (session %s)%s: its CLI and %d processes it left", s.Name, s.ID, how, max(n-1, 0))
}

// listOrNone is the items comma-separated, "none" without any.
func listOrNone(items []string) string {
	if len(items) == 0 {
		return ghNone
	}
	return strings.Join(items, ", ")
}

// resume lifts the standing stop. It first starts the units the stop
// stopped, while the stop still stands: one that does not start keeps the
// stop, with that unit still to start, and is an error. Then it lifts the
// holds the stop set, puts back those it replaced and clears the record.
func (a *app) resume(ctx context.Context) error {
	me, err := a.caller()
	if err != nil {
		return err
	}
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
	if st.Stop == nil {
		p("resume: beekeeper is not stopped")
		return nil
	}
	var startedUnits, failed []string
	for _, u := range st.Stop.Units {
		if err := plat.Launcher.Resume(ctx, u); err != nil {
			failed = append(failed, fmt.Sprintf("starting %s again: %v", u, err))
			continue
		}
		startedUnits = append(startedUnits, u)
		p("resume: started %s again", u)
	}
	var rec *state.Stop
	var lifted, restored, lapsed []string
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		if rec = st.Stop; rec == nil {
			return nil, nil
		}
		if len(failed) > 0 {
			rec.Units = slices.DeleteFunc(rec.Units, func(u string) bool { return slices.Contains(startedUnits, u) })
			return []state.Event{event(me, "resume.failed", "%s", strings.Join(failed, "; "))}, nil
		}
		lifted, restored, lapsed = nil, nil, nil
		st.Holds = slices.DeleteFunc(st.Holds, func(h state.Hold) bool {
			if slices.Contains(rec.Holds, h.Target) && h.Reason == stopHoldReason(rec.Reason) {
				lifted = append(lifted, h.Target)
				return true
			}
			return h.Expired(a.now)
		})
		for _, h := range rec.Replaced {
			switch {
			case h.Expired(a.now):
				lapsed = append(lapsed, holdTarget(h))
			case slices.ContainsFunc(st.Holds, func(x state.Hold) bool { return x.Target == h.Target }):
				// set anew during the stop: that one stays
			default:
				st.Holds = append(st.Holds, h)
				restored = append(restored, holdTarget(h))
			}
		}
		st.Stop = nil
		return []state.Event{event(me, "resume", "the stop by %s since %s: %s", rec.By.Name, rec.At.Format("15:04"), rec.Reason)}, nil
	})
	if err != nil {
		return err
	}
	if rec == nil {
		p("resume: beekeeper is not stopped")
		return nil
	}
	if len(failed) > 0 {
		for _, l := range failed {
			p("resume: FAILED %s", l)
		}
		return fmt.Errorf("beekeeper stays stopped: %d of the units the stop stopped did not start again (above); act on them and run beekeeper resume again", len(failed))
	}
	p("resume: lifted the stop by %s since %s: %s", rec.By.Name, clock(a.now, rec.At), rec.Reason)
	for _, t := range lifted {
		p("resume: lifted the hold on %s", t)
	}
	for _, t := range restored {
		p("resume: put back the hold on %s", t)
	}
	for _, t := range lapsed {
		p("resume: the hold on %s ended during the stop, not put back", t)
	}
	p("resume: capacity as configured: floor %d, ceiling %d", a.cfg.Capacity.Floor, a.cfg.Capacity.Ceiling)
	if len(rec.Sessions) > 0 {
		p("resume: the sessions the stop ended wait for a wake (beekeeper agents wake): %s", strings.Join(rec.Sessions, ", "))
	}
	return nil
}

// stoppedNothing is what a watch's revive, successor or reopen says while
// beekeeper is stopped.
const stoppedNothing = "beekeeper is stopped: nothing revives, starts or reopens until beekeeper resume"

// sayStop says the standing stop once, with its reason, and ENDED once
// beekeeper resume lifted it.
func (w *watcher) sayStop(st *state.State) {
	if st.Stop == nil {
		w.clear("stop")
		return
	}
	w.check("stop", true, "STOPPED: %s; the sessions it ended: %s", w.stopText(st.Stop), listOrNone(st.Stop.Sessions))
}
