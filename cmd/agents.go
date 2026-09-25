package cmd

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

type agentView struct {
	state.Agent
	// Reachable: "live" (the CLI runs), "waiting <t> left" (a bounded wait
	// keeps it reachable), or "not running" (paused or closed: only the
	// person reopening it brings it back).
	Reachable string `json:"reachable"`
}

func (a *app) agentsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "agents",
		Short: "The roster of empty sessions registered as spare capacity",
		Long: `Empty sessions register as spare capacity; the supervisor hands them tasks
before it spawns new sessions. An agent registers, gets a task assigned,
reports back idle, and clears its context between tasks.

Without a subcommand, lists the agents.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return a.agentList() },
	}
	var name string
	register := &cobra.Command{
		Use:   "register",
		Short: "Register the calling session as an idle agent",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			if me.Session == "" {
				return refused("an agent is a Claude Code session: run this inside one, without --as")
			}
			if name != "" {
				me.Name = name
			}
			sessions, _, err := a.sessions()
			if err != nil {
				return err
			}
			live := func(p state.Party) bool {
				_, ok := claude.Live(sessions, p)
				return ok
			}
			var replaced []state.Agent
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				var err error
				replaced, err = registerAgent(st, me, live, a.now.UTC())
				if err != nil {
					return nil, err
				}
				detail := me.Name
				for _, r := range replaced {
					detail += fmt.Sprintf(", replaces %s", r.Session)
				}
				return []state.Event{event(me, "agents.register", "%s", detail)}, nil
			})
			if err != nil {
				return err
			}
			for _, r := range replaced {
				unfinished := ""
				if r.Task != "" {
					unfinished = fmt.Sprintf(", its task unfinished: %q", r.Task)
				}
				_, _ = fmt.Fprintf(a.out, "register: replaces the entry of session %s, which no longer runs%s\n", r.Session, unfinished)
			}
			_, err = fmt.Fprintf(a.out, "register: %s idle, ready for a task\n", me.Name)
			return err
		},
	}
	register.Flags().StringVar(&name, "name", "", "the name to register under (default: the session's title)")
	assign := &cobra.Command{
		Use:   "assign <agent> <task>",
		Short: "Record the task handed to an agent",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			task := strings.Join(args[1:], " ")
			var who string
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				i, err := findAgent(st, args[0])
				if err != nil {
					return nil, err
				}
				ag := &st.Agents[i]
				if ag.Task != "" {
					return nil, refused("%q is busy with %q since %s", ag.Name, ag.Task, clock(a.now, ag.AssignedAt))
				}
				ag.Task, ag.AssignedAt = task, a.now.UTC()
				who = ag.Name
				return []state.Event{event(me, "agents.assign", "%s: %s", who, task)}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "assigned to %s: %s\n", who, task)
			return err
		},
	}
	idle := &cobra.Command{
		Use:   "idle",
		Short: "Report the calling agent's task done: idle again",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				i := slices.IndexFunc(st.Agents, func(x state.Agent) bool { return x.Is(me) })
				if i < 0 {
					return nil, refused("this session is not registered: `beekeeper agents register` first")
				}
				ag := &st.Agents[i]
				ag.LastTask, ag.Task, ag.IdleSince = ag.Task, "", a.now.UTC()
				return []state.Event{event(me, "agents.idle", "%s done: %s", ag.Name, ag.LastTask)}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "register: %s idle, ready for a task\n", me.Name)
			return err
		},
	}
	remove := &cobra.Command{
		Use:   "remove <agent>",
		Short: "Take an agent off the roster",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			var who string
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				i, err := findAgent(st, args[0])
				if err != nil {
					return nil, err
				}
				who = st.Agents[i].Name
				st.Agents = slices.Delete(st.Agents, i, i+1)
				return []state.Event{event(me, "agents.remove", "%s", who)}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "removed %s\n", who)
			return err
		},
	}
	list := listCmd("List the agents, idle ones first", a.agentList)
	c.AddCommand(register, assign, idle, remove, list)
	return c
}

// registerAgent puts me on the roster as an idle agent and returns the
// entries it replaces. A name is one agent's: me replaces its own entry and
// the entries under its name of sessions that no longer run, and is refused
// a name the entry of a running session holds.
func registerAgent(st *state.State, me state.Party, live func(state.Party) bool, now time.Time) ([]state.Agent, error) {
	var replaced []state.Agent
	for _, x := range st.Agents {
		if x.Is(me) || !strings.EqualFold(x.Name, me.Name) {
			continue
		}
		if live(x.Party) {
			return nil, refused("%q is the name of the running session %s: register under another with --name", x.Name, x.Session)
		}
		replaced = append(replaced, x)
	}
	st.Agents = slices.DeleteFunc(st.Agents, func(x state.Agent) bool { return x.Is(me) || strings.EqualFold(x.Name, me.Name) })
	st.Agents = append(st.Agents, state.Agent{Party: me, Registered: now, IdleSince: now})
	return replaced, nil
}

func findAgent(st *state.State, q string) (int, error) {
	parties := make([]state.Party, len(st.Agents))
	for i, ag := range st.Agents {
		parties[i] = ag.Party
	}
	return findParty(parties, q, "registered agent", "agents")
}

// findParty finds q among parties: a session id, a name, or a unique part
// of one. A name several parties carry names none of them.
func findParty(parties []state.Party, q, one, many string) (int, error) {
	lq := strings.ToLower(q)
	var named, hits []int
	for i, p := range parties {
		if q != "" && (p.Session == q || p.HostSession == q) {
			return i, nil
		}
		switch {
		case strings.ToLower(p.Name) == lq:
			named = append(named, i)
		case strings.Contains(strings.ToLower(p.Name), lq):
			hits = append(hits, i)
		}
	}
	switch {
	case len(named) == 1:
		return named[0], nil
	case len(named) > 1:
		ids := make([]string, len(named))
		for i, n := range named {
			ids[i] = parties[n].Session
		}
		return -1, refused("%q names %d %s: pass one's session id (%s)", q, len(named), many, strings.Join(ids, ", "))
	case len(hits) == 1:
		return hits[0], nil
	case len(hits) == 0:
		return -1, refused("no %s matches %q", one, q)
	}
	return -1, refused("%q matches %d %s", q, len(hits), many)
}

func (a *app) agentViews(st *state.State, sessions []*claude.Session) []agentView {
	out := make([]agentView, 0, len(st.Agents))
	for _, ag := range st.Agents {
		v := agentView{Agent: ag, Reachable: "not running"}
		if s, ok := claude.Live(sessions, ag.Party); ok {
			v.Reachable = "live"
			for _, c := range s.Commands {
				if c.Remaining > 0 {
					v.Reachable = "waiting, " + dur(c.Remaining) + " left"
				}
			}
		}
		out = append(out, v)
	}
	slices.SortStableFunc(out, func(x, y agentView) int {
		return boolInt(x.Task != "") - boolInt(y.Task != "")
	})
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (a *app) agentList() error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	sessions, _, err := a.sessions()
	if err != nil {
		return err
	}
	views := a.agentViews(st, sessions)
	if a.json {
		return a.printJSON(views)
	}
	a.printAgents(views)
	return nil
}

func (a *app) printAgents(views []agentView) {
	if len(views) == 0 {
		_, _ = fmt.Fprintln(a.out, "no agent is registered")
		return
	}
	w := a.table()
	_, _ = fmt.Fprintln(w, "AGENT\tTASK\tSINCE\tREACHABLE")
	for _, v := range views {
		task, since := "(idle)", v.IdleSince
		if v.Task != "" {
			task, since = v.Task, v.AssignedAt
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", truncate(v.Name, 30), truncate(task, 60), clock(a.now, since), v.Reachable)
	}
	_ = w.Flush()
}
