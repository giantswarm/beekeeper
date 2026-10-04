package cmd

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
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

--on names what settles the wait: a note (#12) or an issue or pull request
(owner/repo#n). Once the note is closed (answered, defaulted, done or
overtaken) or the pull request merged or closed, the watch says AGENT
RESUMABLE once. With agents.autoResume the watch then resumes the agent
with what settled it, a person's answer word for word, as agents wake
does: by name to a running CLI, else headless. agents resume does the same
by hand, also for a park without --on. A new task or agents idle ends the
park. An agent without a task is refused: it has nothing to wait for.`,
		Example: `  beekeeper agents park --on 41 "Timo's answer on the rollout window"
  beekeeper agents park --on giantswarm/beekeeper#371 "the merge ahead of mine in the lane"
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
			id, isNote := parkNote(on)
			if on != "" && !isNote {
				r, ok := parseRef(on)
				if !ok {
					return usageErr("--on %s is neither a note (#12) nor an issue or pull request (owner/repo#n)", on)
				}
				on = refName(r)
			}
			var line string
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
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
						return nil, refused("note #%d is not open: its answer is in beekeeper log --verb %s", id, noteAnswered)
					}
					on = fmt.Sprintf("#%d", id)
				}
				ag.Park = &state.Park{By: me, At: a.now.UTC(), On: on, Waits: waits}
				for j := range st.Records {
					if st.Records[j].Session.Is(me) {
						st.Records[j].Waits = waits
					}
				}
				back := "agents resume brings it back"
				switch {
				case on != "" && a.cfg.Agents.AutoResume:
					back = "the watch resumes it with what settles " + on
				case on != "":
					back = "the watch says AGENT RESUMABLE once " + on + " settles; agents resume brings it back"
				}
				line = fmt.Sprintf("%s: %s; counted parked, not busy, and %s. End your turn now.", ag.Name, parkedText(ag.Park, a.now), back)
				return []state.Event{event(me, parkEvent, "%s: %s", ag.Name, parkedText(ag.Park, a.now))}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, line)
			return err
		},
	}
	c.Flags().StringVar(&on, "on", "", "what settles the wait: a note (#12) or an issue or pull request (owner/repo#n)")
	return c
}

func (a *app) agentResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <agent>",
		Short: "Resume a parked agent with what settled its wait, or by hand",
		Long: `resume ends an agent's park (agents park) and wakes it, as agents wake
does: by name to a running CLI, else headless. Its turn carries what
settled the wait, a person's answer word for word, or that the caller
resumed it by hand. An agent that is not parked is refused: agents wake
messages it.`,
		Example: `  beekeeper agents resume "BK 86"`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			by, err := a.caller()
			if err != nil {
				return err
			}
			return a.resumeParked(cmd.Context(), by, args[0])
		},
	}
}

// resumeParked ends the park of agent q and wakes it with parkMessage. The
// park ends before the wake, so two resumes never wake it twice, and comes
// back when the wake fails.
func (a *app) resumeParked(ctx context.Context, by state.Party, q string) error {
	var ag state.Agent
	var p *state.Park
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		i, err := findAgent(st, q)
		if err != nil {
			return nil, err
		}
		if st.Agents[i].Park == nil {
			return nil, refused("%q is not parked: agents wake messages it", st.Agents[i].Name)
		}
		p, st.Agents[i].Park = st.Agents[i].Park, nil
		ag = st.Agents[i]
		return []state.Event{event(by, resumedEvent, "%s: %s", ag.Name, parkedText(p, a.now))}, nil
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

// settledParks reads which parks of st settled: a note it parked on is no
// longer open (the log, read only then, has its answer), an issue or pull
// request is merged or closed (GitHub, read only for parks on one and while
// the budget is over its floor).
func (w *watcher) settledParks(ctx context.Context, st *state.State) []settledPark {
	open := map[int]bool{}
	for _, n := range st.Notes {
		open[n.ID] = true
	}
	var parked []state.Agent
	var closed []int
	var refs []github.PR
	for _, ag := range st.Agents {
		p := ag.Park
		if p == nil || p.On == "" || !p.Resumable.IsZero() {
			continue
		}
		if id, ok := parkNote(p.On); ok {
			if !open[id] {
				parked, closed = append(parked, ag), append(closed, id)
			}
		} else if r, ok := parseRef(p.On); ok {
			parked = append(parked, ag)
			if !slices.Contains(refs, r) {
				refs = append(refs, r)
			}
		}
	}
	if len(parked) == 0 {
		return nil
	}
	var states map[github.PR]string
	if len(refs) > 0 && !lowBudget(st.Budget, w.cfg.GitHub.Floor, w.now) {
		s, err := refStates(ctx, refs)
		w.check("park-refs", s == nil, "cannot read the parked agents' issues and pull requests: %v", err)
		states = s
	}
	closes := map[int]string{}
	if len(closed) > 0 {
		evs, err := w.store.Events(0, func(e state.Event) bool {
			switch e.Verb {
			case noteAnswered, noteDefaulted, noteDone, noteOvertaken, noteReplaced:
				return true
			}
			return false
		})
		if err != nil {
			return nil // the next poll reads them again
		}
		for _, e := range evs {
			for _, id := range closed {
				if !strings.HasPrefix(e.Detail, fmt.Sprintf("#%d ", id)) {
					continue
				}
				closes[id] = fmt.Sprintf("note #%d closed: %s", id, oneLine(e.Detail))
				if an, ok := parseAnswered(e); ok {
					closes[id] = fmt.Sprintf("note #%d answered by %s: %s", id, cmp.Or(e.By.Person, e.By.Name), an.Answer)
				}
			}
		}
	}
	var out []settledPark
	for _, ag := range parked {
		on := ag.Park.On
		if id, ok := parkNote(on); ok {
			out = append(out, settledPark{ag.Party, on, cmp.Or(closes[id], fmt.Sprintf("note #%d is closed", id))})
			continue
		}
		r, _ := parseRef(on)
		if s := states[r]; s != "" && s != github.Open {
			out = append(out, settledPark{ag.Party, on, refName(r) + " " + strings.ToLower(s)})
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
	if !w.cfg.Agents.AutoResume {
		return
	}
	for _, ag := range st.Agents {
		if ag.Park == nil || ag.Park.Resumable.IsZero() {
			continue
		}
		quiet := *w.app // the wake's own report is not a watch line
		quiet.out = io.Discard
		if err := quiet.resumeParked(ctx, watchParty, cmp.Or(ag.Session, ag.Name)); err != nil {
			w.emit("park-resume-"+ag.Name, "AGENT RESUME FAILED %q: %v; beekeeper agents resume %q", ag.Name, err, ag.Name)
		}
	}
}
