package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
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
var turnPatterns = []string{"beekeeper-agent-*", "beekeeper-wake-*", reopenUnitPrefix + "*", "beekeeper-report-*"}

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
- holds "merges" and "github" (beekeeper hold), unless held already;
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
keeps its first reason. It prints what it stopped.`,
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
		Long: `Lift the standing beekeeper stop: the holds the stop set are lifted (a
hold that stood before it stays), the installed units it stopped (the
standby service, stop.units) start again, and starts, wakes, revives,
reopens and imports run again. The capacity is what it was: the stop never
changes capacity.floor or capacity.ceiling. A wake timer that fell due
during the stop fires at the watch's next poll.

It starts no session: it names the sessions the stop ended, which wait for
a wake (beekeeper agents wake), and touches no session the person started.
It prints what it lifted and restarted.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.resume(cmd.Context()) },
	}
}

// stop records the stop and its holds, then stops the units and ends the
// sessions it stops, and records those.
func (a *app) stop(ctx context.Context, reason string) error {
	me, err := a.caller()
	if err != nil {
		return err
	}
	var rec state.Stop
	var again bool
	var held []string
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		if again = st.Stop != nil; !again {
			st.Stop = &state.Stop{Reason: reason, By: me, At: a.now.UTC()}
		}
		held = nil
		for _, t := range stopHolds {
			// A hold that stood before the stop, or that its first run set,
			// stays; one that lets a merge through (--except, a tool-release
			// window) gives way to the stop's.
			if slices.ContainsFunc(st.Holds, func(h state.Hold) bool {
				return h.Target == t && h.Active(a.now) && h.Except == "" && h.ExceptSession == "" && h.Tool == ""
			}) {
				continue
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
	for _, t := range held {
		p("stop: held %s", t)
	}
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	spare := spared(st, me)
	units, lines := a.stopUnits(ctx, spare)
	for _, l := range lines {
		p("stop: %s", l)
	}
	ended, lines := a.endWorkers(ctx, st, spare)
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
		return []state.Event{event(me, "stop.stopped", "units %s; sessions %s", listOrNone(units), listOrNone(ended))}, nil
	})
	if err != nil {
		return err
	}
	p("stop: nothing starts, wakes, revives, imports or merges until beekeeper resume")
	return nil
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
// stopped, which resume starts again, and a line for each unit.
func (a *app) stopUnits(ctx context.Context, spare []state.Party) (installed, lines []string) {
	if !plat.Launcher.Available() {
		return nil, []string{"no unit stopped: this machine has no unit launcher (systemd --user); the standby watch honours the stop"}
	}
	own := ownUnit()
	for _, u := range plat.Launcher.Running(ctx, true, turnPatterns...) {
		if sparedUnit(u, own, spare) {
			continue
		}
		if err := plat.Launcher.Stop(ctx, u); err != nil {
			lines = append(lines, fmt.Sprintf("stopping %s: %v", u, err))
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
			lines = append(lines, fmt.Sprintf("stopping %s: %v", u, err))
			continue
		}
		installed = append(installed, u)
		lines = append(lines, "stopped "+u+"; resume starts it again")
	}
	return installed, lines
}

// endWorkers ends every live session beekeeper started but the spared
// ones, in parallel, and returns the names it ended and a line for each.
func (a *app) endWorkers(ctx context.Context, st *state.State, spare []state.Party) (ended, lines []string) {
	sessions, _, err := a.sessions()
	if err != nil {
		return nil, []string{fmt.Sprintf("no session ended: reading the sessions failed: %v", err)}
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
				lines = append(lines, fmt.Sprintf("ending %q (session %s): %v", s.Name, s.ID, err))
				return
			}
			ended = append(ended, s.Name)
			lines = append(lines, endedLine(s, n))
		})
	}
	wg.Wait()
	slices.Sort(ended)
	slices.Sort(lines)
	return ended, lines
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

// resume lifts the standing stop: its holds, then the units it stopped.
func (a *app) resume(ctx context.Context) error {
	me, err := a.caller()
	if err != nil {
		return err
	}
	var rec *state.Stop
	var lifted []string
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		if rec = st.Stop; rec == nil {
			return nil, nil
		}
		st.Holds = slices.DeleteFunc(st.Holds, func(h state.Hold) bool {
			if slices.Contains(rec.Holds, h.Target) && h.Reason == stopHoldReason(rec.Reason) {
				lifted = append(lifted, h.Target)
				return true
			}
			return h.Expired(a.now)
		})
		st.Stop = nil
		return []state.Event{event(me, "resume", "the stop by %s since %s: %s", rec.By.Name, rec.At.Format("15:04"), rec.Reason)}, nil
	})
	if err != nil {
		return err
	}
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
	if rec == nil {
		p("resume: beekeeper is not stopped")
		return nil
	}
	p("resume: lifted the stop by %s since %s: %s", rec.By.Name, clock(a.now, rec.At), rec.Reason)
	for _, t := range lifted {
		p("resume: lifted the hold on %s", t)
	}
	for _, u := range rec.Units {
		if err := plat.Launcher.Resume(ctx, u); err != nil {
			p("resume: starting %s again: %v", u, err)
			continue
		}
		p("resume: started %s again", u)
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
