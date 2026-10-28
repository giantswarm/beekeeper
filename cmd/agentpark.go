package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/feed"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/upgrade"
)

// The events of a park: set, settled (its note closed or its pull request
// merged or closed) and ended by a resume.
const (
	parkEvent      = "agents.park"
	resumableEvent = "agents.resumable"
	resumedEvent   = "agents.resumed"
)

// parkNote reads a park's On as a note id ("#12" or "12").
func parkNote(on string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(on, "#"))
	return n, err == nil && n > 0
}

// parkedText says what a park waits on, and once settled, since when it is
// resumable and why.
func parkedText(p *state.Park, now time.Time) string {
	s := "parked"
	if p.On != "" {
		s += " on " + p.On
	}
	s += ": " + p.Waits
	if !p.Resumable.IsZero() {
		s += fmt.Sprintf("; resumable since %s: %s", clock(now, p.Resumable), p.Answer)
	}
	return s
}

// parkMessage is the turn a parked agent is resumed with: what settled its
// wait word for word, or who resumed it by hand.
func parkMessage(p *state.Park, by string) string {
	what := p.Waits
	if p.On != "" {
		what = fmt.Sprintf("%s (%s)", p.On, p.Waits)
	}
	why := p.Answer
	if p.Resumable.IsZero() {
		why = by + " resumed you by hand"
	}
	return fmt.Sprintf("Your park on %s is over: %s. Re-query the live state of your task and continue it.", what, why)
}

func (a *app) agentParkCmd() *cobra.Command {
	var on string
	c := &cobra.Command{
		Use:   "park <what it waits for>",
		Short: "Park the calling agent on a note, a pull request or a person: it keeps its task without holding a busy slot",
		Long: `park marks the calling agent parked: it keeps its task, but agents and
capacity count it parked, not busy, the doctor and the watch leave it
alone, and it ends its turn instead of waiting in a sleep that holds its
slot and its CLI's memory.

--on names what settles the wait: a note (#12; on a machine with a central
instance <context>#12 is its note, and a bare number the machine has no
note of is the central instance's), an issue or pull request
(owner/repo#n) or a hold (hold:<target>, the one a gate's exit 77 names);
lease claim --park parks on a lease. Once the note is closed (answered,
defaulted, done or overtaken), the pull request merged or closed, the hold
lifted or ended, or the lease free and the agent's to claim, the watch says AGENT
RESUMABLE once. With agents.autoResume the watch then resumes the agent
with what settled it, a person's answer word for word, as agents wake
does: by name to a running CLI, else headless. agents resume does the same
by hand, also for a park without --on. A new task or agents idle ends the
park. An agent without a task is refused: it has nothing to wait for. One
that reported its task idle (agents idle, --done included) has a task
again once the supervisor's next message re-tasks it, by SendMessage or
agents wake: its task is that message's first line.`,
		Example: `  beekeeper agents park --on 41 "Timo's answer on the rollout window"
  beekeeper agents park --on giantswarm/beekeeper#371 "the merge ahead of mine in the lane"
  beekeeper agents park --on hold:merges "the devctl release window my merge was refused on"
  beekeeper agents park "the supervisor's go"`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			waits := oneLine(args[0])
			if waits == "" {
				return usageErr("say what the agent waits for")
			}
			if target, isHold := strings.CutPrefix(on, holdParkFlag); isHold {
				if target = strings.TrimSpace(target); target == "" {
					return usageErr("--on %s names no hold: hold:<target>, as beekeeper hold lists it", on)
				}
				on = holdParkOn(target)
			} else if _, isNote := parkNote(on); on != "" && !isNote {
				if _, _, central := parkCentralNote(on); !central {
					r, ok := parseRef(on)
					if !ok {
						return usageErr("--on %s is neither a note (#12, <context>#12), an issue or pull request (owner/repo#n) nor a hold (hold:<target>)", on)
					}
					on = refName(r)
				}
			}
			line, err := a.park(me, on, waits)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, line)
			return err
		},
	}
	c.Flags().StringVar(&on, "on", "", "what settles the wait: a note (#12, <context>#12 for a central instance's), an issue or pull request (owner/repo#n) or a hold (hold:<target>)")
	return c
}

// park parks the agent me on on (a note "#12", a central instance's note
// "<context>#12", an issue or pull request owner/repo#n, a hold of
// holdParkOn, a lease of leaseParkOn, or nothing) waiting for waits, and
// returns the line that says so. A bare note number is the central
// instance's note when the machine has none of it open, as note answer's.
func (a *app) park(me state.Party, on, waits string) (string, error) {
	id, isNote := parkNote(on)
	if _, _, central := parkCentralNote(on); isNote || central {
		var err error
		if on, err = a.parkNoteOn(on); err != nil {
			return "", err
		}
		id, isNote = parkNote(on)
	}
	var line string
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(me) })
		if i < 0 {
			return nil, refused("%s is no registered agent: beekeeper agents register first", me.Name)
		}
		ag := &st.Agents[i]
		if ag.Task == "" || ag.Done {
			return nil, refused("%q has no task: park keeps a task, an idle agent has nothing to wait for", ag.Name)
		}
		if isNote {
			if !slices.ContainsFunc(st.Notes, func(n state.Note) bool { return n.ID == id }) {
				return nil, refused("note #%d is not open: its answer is in beekeeper log --verb %s", id, answerRecorded)
			}
			on = fmt.Sprintf("#%d", id)
		}
		if target, ok := parkHold(on); ok && !holdStands(st, target, a.now) {
			return nil, refused("nothing holds %s: run the refused command again, there is nothing to park on", target)
		}
		ag.Park = &state.Park{By: me, At: a.now.UTC(), On: on, Waits: waits}
		for j := range st.Records {
			if st.Records[j].Session.Is(me) {
				st.Records[j].Waits = waits
			}
		}
		resume := fmt.Sprintf("beekeeper agents resume %q", ag.Name)
		back := resume + " brings it back"
		switch {
		case on != "" && a.cfg.Agents.AutoResume:
			back = "the watch resumes it with what settles " + on
		case on != "":
			back = "the watch says AGENT RESUMABLE once " + on + " settles; " + resume + " brings it back"
		}
		line = fmt.Sprintf("%s: %s; counted parked, not busy, and %s. End your turn now.", ag.Name, parkedText(ag.Park, a.now), back)
		return []state.Event{event(me, parkEvent, "%s: %s", ag.Name, parkedText(ag.Park, a.now))}, nil
	})
	return line, err
}

// leaseParkOn is the On of a park on the lease res (lease claim --park):
// it settles once res is free and the agent may claim it.
func leaseParkOn(res string) string { return "lease " + res }

// parkLease reads a park's On as a lease's resource.
func parkLease(on string) (string, bool) {
	res, ok := strings.CutPrefix(on, "lease ")
	return res, ok && res != ""
}

// holdParkFlag prefixes a hold's target in park --on.
const holdParkFlag = "hold:"

// holdParkOn is the On of a park on the hold of target: it settles once
// that hold is lifted or its time passed.
func holdParkOn(target string) string { return "hold " + target }

// parkHold reads a park's On as a hold's target.
func parkHold(on string) (string, bool) {
	target, ok := strings.CutPrefix(on, "hold ")
	return target, ok && target != ""
}

// holdStands says whether an active hold of target is in st at now.
func holdStands(st *state.State, target string, now time.Time) bool {
	return slices.ContainsFunc(st.Holds, func(h state.Hold) bool { return h.Target == target && h.Active(now) })
}

// leaseSettled says whether the lease res, which ag's park waits on, is
// free and ag's to claim, and the words that resume it: free while nobody
// holds it, ag's while no upgrade holds it and ag's grant is first in its
// queue (or no supervisor is recorded); a grant to ag that is gone settles
// the park too, for ag to ask again, also while the lease is held (the
// supervisor revoked it). A grant to another session first, or a holder
// while ag's grant waits, leaves it parked.
func (a *app) leaseSettled(st *state.State, ag state.Agent, res string) (string, bool) {
	cur, err := lease.Dir(a.cfg.LeaseDir).Get(res)
	if err != nil {
		return "", false
	}
	if cur != nil {
		if st.SupervisorRole().Holder == nil || slices.ContainsFunc(lease.Pending(st, res, true, a.now, a.cfg.GrantTTL.Duration), func(g state.Grant) bool { return g.To.Is(ag.Party) }) {
			return "", false
		}
		return fmt.Sprintf("%s is held by %q and no grant of it to you is left: ask the supervisor `need %s` again", res, cur.Name, res), true
	}
	if _, ok := upgrade.Held(st, res, a.now); ok {
		return "", false
	}
	_, err = lease.Check(st, lease.Gate{Resource: res, Caller: ag.Party, Supervisor: st.SupervisorRole().Holder, Now: a.now, TTL: a.cfg.GrantTTL.Duration})
	claim := fmt.Sprintf("beekeeper lease claim %s -p \"<purpose>\"", res)
	var r *lease.Refusal
	switch {
	case err == nil:
		return fmt.Sprintf("%s is free and yours: claim it now with `%s`, within %s", res, claim, dur(a.cfg.GrantTTL.Duration)), true
	case errors.As(err, &r) && r.Position > 0:
		return "", false
	}
	return fmt.Sprintf("%s is free, but no grant of it to you is left: ask the supervisor `need %s` again", res, res), true
}

func (a *app) agentResumeCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   agentResumeName + " [--force] <agent>",
		Short: "Resume a parked agent with what settled its wait, or by hand",
		Long: `resume ends an agent's park (agents park) and wakes it, as agents wake
does: by name to a running CLI, else headless. Its turn carries what
settled the wait, a person's answer word for word, or that the caller
resumed it by hand. An agent that is not parked is refused: agents wake
messages it. While USAGE HIGH holds on the 5-hour window
(capacity.usage.highPercent), resume refuses until fiveHour.resetsAt
unless --force is given.`,
		Example: `  beekeeper agents resume "BK 86"`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			by, err := a.caller()
			if err != nil {
				return err
			}
			return a.resumeParkedOpts(cmd.Context(), by, args[0], force)
		},
	}
	c.Flags().BoolVar(&force, "force", false, "resume even while USAGE HIGH holds")
	return c
}

// resumeParked ends the park of agent q and wakes it with parkMessage. The
// park ends before the wake, so two resumes never wake it twice, and comes
// back when the wake fails.
func (a *app) resumeParked(ctx context.Context, by state.Party, q string) error {
	return a.resumeParkedOpts(ctx, by, q, false)
}

func (a *app) resumeParkedOpts(ctx context.Context, by state.Party, q string, force bool) error {
	var target *state.Agent
	st, err := a.store.Read()
	if err == nil {
		i, ferr := findAgent(st, q)
		if ferr != nil {
			return ferr
		}
		if st.Agents[i].Park == nil {
			return refused("%q is not parked: agents wake messages it", st.Agents[i].Name)
		}
		cp := st.Agents[i]
		target = &cp
	}
	block := a.usageHighBlockForAgent(ctx, st, nil, target)
	if block != "" && !force {
		return refused("no resume: %s (--force resumes anyway)", block)
	}
	var ag state.Agent
	var p *state.Park
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		i, err := findAgent(st, q)
		if err != nil {
			return nil, err
		}
		if st.Agents[i].Park == nil {
			return nil, refused("%q is not parked: agents wake messages it", st.Agents[i].Name)
		}
		p, st.Agents[i].Park = st.Agents[i].Park, nil
		ag = st.Agents[i]
		detail := fmt.Sprintf("%s: %s", ag.Name, parkedText(p, a.now))
		if block != "" {
			detail += fmt.Sprintf(" (over no resume: %s; --force)", block)
		}
		return []state.Event{event(by, resumedEvent, "%s", detail)}, nil
	})
	if err != nil {
		return err
	}
	if err := wakeOwner(a, ctx, by, ag.Name, parkMessage(p, by.Name), ""); err != nil {
		_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
			for k := range st.Agents {
				if st.Agents[k].Is(ag.Party) && st.Agents[k].Park == nil && st.Agents[k].Task == ag.Task {
					st.Agents[k].Park = p
				}
			}
			return nil, nil
		})
		return fmt.Errorf("resuming %s: %w; it stays parked", ag.Name, err)
	}
	return nil
}

// settledPark is a park whose note closed or whose issue or pull request
// merged or closed, and what settled it.
type settledPark struct {
	party      state.Party
	on, answer string
}

// settledParks reads which parks of st settled: a hold it parked on is
// lifted or ended, a note it parked on is no longer open (the log, read only then, has its answer), an issue or pull
// request is merged or closed (GitHub, read only for parks on one and while
// the budget is over its floor).
func (w *watcher) settledParks(ctx context.Context, st *state.State) []settledPark {
	open := map[int]bool{}
	for _, n := range st.Notes {
		open[n.ID] = true
	}
	var parked, centralParked []state.Agent
	var closed []int
	var refs []github.PR
	var out []settledPark
	for _, ag := range st.Agents {
		p := ag.Park
		if p == nil || p.On == "" || !p.Resumable.IsZero() {
			continue
		}
		if res, ok := parkLease(p.On); ok {
			if answer, ok := w.leaseSettled(st, ag, res); ok {
				out = append(out, settledPark{ag.Party, p.On, answer})
			}
			continue
		}
		if target, ok := parkHold(p.On); ok {
			if !holdStands(st, target, w.now) {
				out = append(out, settledPark{ag.Party, p.On, fmt.Sprintf("nothing holds %s any more: run the refused command again", target)})
			}
			continue
		}
		if id, ok := parkNote(p.On); ok {
			if !open[id] {
				parked, closed = append(parked, ag), append(closed, id)
			}
		} else if ctx, _, ok := parkCentralNote(p.On); ok {
			if w.centralNotesOn() && strings.EqualFold(ctx, w.cfg.Central.Context) {
				centralParked = append(centralParked, ag)
			}
		} else if r, ok := parseRef(p.On); ok {
			parked = append(parked, ag)
			if !slices.Contains(refs, r) {
				refs = append(refs, r)
			}
		}
	}
	out = append(out, w.settledCentralParks(ctx, centralParked)...)
	if len(parked) == 0 {
		return out
	}
	var states map[github.PR]github.RefState
	if len(refs) > 0 && !lowBudget(st.Budget, w.cfg.GitHub.Floor, w.now) {
		// A failed batch leaves its agents parked; the others settle.
		s, err := refStates(ctx, refs)
		if err != nil {
			w.fail("park-refs", "cannot read the parked agents' issues and pull requests: %v", err)
		} else {
			w.clear("park-refs")
		}
		states = s
	}
	var closes map[int]string
	if len(closed) > 0 {
		evs, err := w.store.Events(0, func(e state.Event) bool { return noteClosing(e.Verb) })
		if err != nil {
			return out // the next poll reads the notes again
		}
		closes = noteCloses(evs, closed, "")
	}
	for _, ag := range parked {
		on := ag.Park.On
		if id, ok := parkNote(on); ok {
			out = append(out, settledPark{ag.Party, on, cmp.Or(closes[id], fmt.Sprintf("note #%d is closed", id))})
			continue
		}
		r, _ := parseRef(on)
		if s := states[r].State; s != "" && s != github.Open {
			out = append(out, settledPark{ag.Party, on, refName(r) + " " + strings.ToLower(s)})
		}
	}
	return out
}

// noteClosing says whether verb closes a note: answered, defaulted, done
// or overtaken.
func noteClosing(verb string) bool {
	switch verb {
	case answerRecorded, noteDefaulted, noteDone, noteOvertaken:
		return true
	}
	return false
}

// noteCloses is what closed each note of ids among the closing events evs,
// oldest first, the last one winning: its answer word for word, or how it
// closed. Each note is named <central>#<n>.
func noteCloses(evs []state.Event, ids []int, central string) map[int]string {
	closes := map[int]string{}
	for _, e := range evs {
		for _, id := range ids {
			if an, ok := parseAnswered(e); ok {
				if an.ID == id {
					closes[id] = fmt.Sprintf("note %s#%d answered by %s: %s", central, id, an.For, an.Answer)
					if an.Comment != "" {
						closes[id] += " (" + an.Comment + ")"
					}
				}
			} else if strings.HasPrefix(e.Detail, fmt.Sprintf("#%d ", id)) {
				closes[id] = fmt.Sprintf("note %s#%d closed: %s", central, id, oneLine(e.Detail))
			}
		}
	}
	return closes
}

// settledCentralParks reads which of parked, each parked on a central
// note, settled: its note is no longer among the central instance's open
// notes (note_list); what closed it is the feed's (read_feed), read only
// then. An unreachable central instance leaves them parked, said once.
func (w *watcher) settledCentralParks(ctx context.Context, parked []state.Agent) []settledPark {
	if len(parked) == 0 {
		return nil
	}
	const key = "park-central"
	me := state.Party{Name: w.cfg.Identity.Person, Host: w.cfg.Identity.Host}
	notes, err := w.centralNotes(ctx, me)
	if err != nil {
		w.fail(key, "cannot read the central instance's notes (%s) the parked agents wait on: %v", w.cfg.Central.Context, err)
		return nil
	}
	open := map[int]bool{}
	for _, n := range notes {
		open[n.ID] = true
	}
	var closed []int
	for _, ag := range parked {
		if _, id, _ := parkCentralNote(ag.Park.On); !open[id] {
			closed = append(closed, id)
		}
	}
	if len(closed) == 0 {
		w.clear(key)
		return nil
	}
	var f feed.Feed
	if _, err := w.callCentralRaw(ctx, me, nameReadFeed, map[string]any{}, &f); err != nil {
		w.fail(key, "cannot read the central feed (%s) for what closed the notes the parked agents wait on: %v", w.cfg.Central.Context, err)
		return nil
	}
	w.clear(key)
	var evs []state.Event
	for _, ev := range f.Events {
		detail, ok := strings.CutPrefix(ev.Line, feed.Line(ev.Kind, ev.Actor, ""))
		if ok && noteClosing(ev.Kind) {
			evs = append(evs, state.Event{At: ev.Time, Verb: ev.Kind, Detail: detail, By: state.Party{Name: ev.Actor.Name, Person: ev.Actor.Person, Host: ev.Actor.Host}})
		}
	}
	closes := noteCloses(evs, closed, w.cfg.Central.Context)
	var out []settledPark
	for _, ag := range parked {
		if _, id, _ := parkCentralNote(ag.Park.On); !open[id] {
			out = append(out, settledPark{ag.Party, ag.Park.On, cmp.Or(closes[id], fmt.Sprintf("note %s is closed", ag.Park.On))})
		}
	}
	return out
}

// markResumable marks the parks of st among settled resumable and returns
// their watch lines and events: one AGENT RESUMABLE line per park, since
// the mark is in the state. A park ended or moved meanwhile is left alone.
func markResumable(st *state.State, settled []settledPark, auto bool, now time.Time) ([]string, []state.Event) {
	var lines []string
	var evs []state.Event
	for _, s := range settled {
		for k := range st.Agents {
			ag := &st.Agents[k]
			if !ag.Is(s.party) || ag.Park == nil || ag.Park.On != s.on || !ag.Park.Resumable.IsZero() {
				continue
			}
			ag.Park.Resumable, ag.Park.Answer = now.UTC(), s.answer
			next := fmt.Sprintf("beekeeper agents resume %q", ag.Name)
			if auto {
				next = "the watch resumes it with these words"
			}
			lines = append(lines, fmt.Sprintf("AGENT RESUMABLE %q: %s; %s", ag.Name, truncate(parkedText(ag.Park, now), 400), next))
			evs = append(evs, event(watchParty, resumableEvent, "%s: %s", ag.Name, parkedText(ag.Park, now)))
		}
	}
	return lines, evs
}

// autoResume resumes, with agents.autoResume, every parked agent of st
// whose park is resumable; a resume that fails is a line, and the agent
// stays parked for the next poll or agents resume.
func (w *watcher) autoResume(ctx context.Context, st *state.State) {
	if !w.cfg.Agents.AutoResume || st.Stop != nil {
		return
	}
	for _, ag := range st.Agents {
		if ag.Park == nil || ag.Park.Resumable.IsZero() {
			continue
		}
		cp := ag
		if w.usageHighBlockForAgent(ctx, st, nil, &cp) != "" {
			continue
		}
		quiet := *w.app // the wake's own report is not a watch line
		quiet.out = io.Discard
		if err := quiet.resumeParked(ctx, watchParty, cmp.Or(ag.Session, ag.Name)); err != nil {
			w.emit("park-resume-"+ag.Name, "AGENT RESUME FAILED %q: %v; beekeeper agents resume %q", ag.Name, err, ag.Name)
		}
	}
}
