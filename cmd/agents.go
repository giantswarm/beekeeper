package cmd

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/state"
)

type agentView struct {
	state.Agent
	// Reachable: "live" (the CLI runs), "live, first turn running" or
	// "live, wake turn running" (a headless turn of agents start or agents
	// wake is its CLI), "waiting <t> left" (a bounded wait keeps it
	// reachable), "not running, import waits until <t>" (its reopen waits
	// to show it in the desktop), or "not running" (paused or closed: agents
	// wake brings it back).
	Reachable string `json:"reachable"`
	// Model is the model its running session is on.
	Model string `json:"model,omitempty"`
	// Browser is how the desktop answers its navigate to a site it was not
	// allowed on yet: browserAsks or browserSkips; empty without a desktop
	// session.
	Browser string `json:"browser,omitempty"`
	// Kept says what keeps the idle entry on the roster past
	// agents.staleAfter (keptBy); empty when nothing does.
	Kept string `json:"kept,omitempty"`
	// HeadlessTurn: a headless turn of agents start or agents wake is its
	// CLI, which Claude Desktop does not run, so its sidebar row shows the
	// session idle while it works.
	HeadlessTurn bool `json:"headlessTurn,omitempty"`
}

// An agent's Browser.
const (
	browserAsks  = "asks"
	browserSkips = "skips"
)

// agentBrowser is the agent's Browser, read from its desktop record.
func agentBrowser(cfg *config.Config, ag state.Agent) string {
	if !strings.HasPrefix(ag.HostSession, "local_") {
		return ""
	}
	r, ok := claude.ReadRecord(cfg, ag.HostSession)
	switch {
	case !ok:
		return ""
	case r.BrowserAsks():
		return browserAsks
	}
	return browserSkips
}

// The agents command's name and its reopen subcommand's, which a start's and
// a wake's unit run once their turn ended.
const (
	agentsName = "agents"
	reopenName = "reopen"
)

func (a *app) agentsCmd() *cobra.Command {
	var full bool
	c := &cobra.Command{
		Use:   agentsName,
		Short: "The roster of empty sessions registered as spare capacity",
		Long: `Empty sessions register as spare capacity; the supervisor hands them tasks
before it spawns new sessions. An agent registers, gets a task assigned,
reports back idle, and clears its context between tasks.

Without a subcommand, lists the agents. BROWSER says how the desktop
answers an agent's navigate to a site it was not allowed on yet: asks (a
site request waits for a person in its desktop row, which no hook answers)
or skips (Chrome permission mode skip_all_permission_checks). A caller that has read the list
before gets only the agents whose task or reachability changed since, or
one "no change" line; --full prints everything. KEPT says what keeps an
entry on the roster past agents.staleAfter: its keep marker with its reason
(agents keep) or a timer that wakes it by name.

This roster is the view of which agents work. A first turn of agents start
and a wake turn of agents wake run headless, outside Claude Desktop: its
sidebar shows such a session idle while REACHABLE says "first turn running"
or "wake turn running".`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return a.agentList(full) },
	}
	fullFlag(c, &full)
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
			var reg registration
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				var err error
				reg, err = registerAgent(st, me, live, a.now.UTC())
				if err != nil {
					return nil, err
				}
				detail := me.Name
				for _, r := range reg.replaced {
					detail += fmt.Sprintf(", replaces %s", r.Session)
				}
				switch {
				case reg.own:
					detail += fmt.Sprintf(", busy with %q", reg.task)
				case reg.task != "":
					detail += fmt.Sprintf(", takes over %q", reg.task)
				}
				return []state.Event{event(me, "agents.register", "%s", detail)}, nil
			})
			if err != nil {
				return err
			}
			for _, r := range reg.replaced {
				_, _ = fmt.Fprintf(a.out, "register: replaces the entry of session %s, which no longer runs\n", r.Session)
			}
			if reg.own {
				_, err = fmt.Fprintf(a.out, "register: %s busy with %q since %s: work it, then `beekeeper agents idle`\n",
					me.Name, reg.task, clock(a.now, reg.assignedAt))
				return err
			}
			if reg.task != "" {
				_, err = fmt.Fprintf(a.out, "register: %s takes over the unfinished task %q, assigned %s: work it, then `beekeeper agents idle`\n",
					me.Name, reg.task, clock(a.now, reg.assignedAt))
				return err
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
				ag.Task, ag.AssignedAt, ag.Done = task, a.now.UTC(), false
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
	var finished bool
	idle := &cobra.Command{
		Use:   "idle",
		Short: "Report the calling agent's task done: idle again",
		Long: `Reports the calling agent's task done: idle again, ready for the next.
With --done its work is finished: the watch's doctor takes it off the
roster and archives the desktop session beekeeper started for it once its
CLI runs no turn, a desktop CLI kept warm included (beekeeper doctor; the
desktop's Archived list brings it back).`,
		Args: cobra.NoArgs,
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
				reportIdle(ag, a.now)
				ag.Done = finished
				verb := "agents.idle"
				if finished {
					verb = "agents.done"
				}
				return []state.Event{event(me, verb, "%s done: %s", ag.Name, ag.LastTask)}, nil
			})
			if err != nil {
				return err
			}
			if finished {
				_, err = fmt.Fprintf(a.out, "register: %s finished: off the roster and archived once its turn ends\n", me.Name)
				return err
			}
			_, err = fmt.Fprintf(a.out, "register: %s idle, ready for a task\n", me.Name)
			return err
		},
	}
	idle.Flags().BoolVar(&finished, "done", false, "the work is finished: the doctor removes and archives the agent once idle")
	var keepDesktop bool
	remove := &cobra.Command{
		Use:   "remove <agent>",
		Short: "Take an agent off the roster and archive the desktop session beekeeper started for it",
		Long: `Takes an agent off the roster. When beekeeper started the agent's session,
its desktop session is archived too (the desktop's Archived list brings it
back), unless it runs a turn or holds the supervisor's or the guide's role: an idle desktop CLI of a session beekeeper started is asked to
archive it. A session its person started is never archived.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			var gone state.Party
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				i, err := findAgent(st, args[0])
				if err != nil {
					return nil, err
				}
				gone = st.Agents[i].Party
				removeAgent(st, i)
				return []state.Event{event(me, "agents.remove", "%s", gone.Name)}, nil
			})
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(a.out, "removed %s\n", gone.Name); err != nil || keepDesktop {
				return err
			}
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			outcomes := a.archiveDesktops(cmd.Context(), st, []state.Party{gone}, "beekeeper agents remove "+gone.Name)
			line := outcomes[0].line
			_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
				line = owe(st, outcomes, a.now)[0]
				return []state.Event{event(me, "agents.archive", "%s: %s", gone.Name, line)}, nil
			})
			_, err = fmt.Fprintf(a.out, "%s: %s\n", gone.Name, line)
			return err
		},
	}
	remove.Flags().BoolVar(&keepDesktop, "keep-desktop", false, "leave the agent's desktop session in the sidebar")
	list := listCmd("List the agents, idle ones first", func() error { return a.agentList(full) })
	fullFlag(list, &full)
	c.AddCommand(register, a.onHost(a.agentStartCmd(), sandbox.OpAgents, agentsBrokeredTimeout+time.Minute), a.onHost(a.agentWakeCmd(), sandbox.OpAgents, agentsBrokeredTimeout+time.Minute), a.agentReopenCmd(), a.agentRoomCmd(), a.agentDesktopCmd(), a.agentHandoverCmd(), a.agentNoteCmd(), a.agentBroadcastCmd(), a.agentKeepCmd(), a.agentParkCmd(), a.onHost(a.agentResumeCmd(), sandbox.OpAgents, agentsBrokeredTimeout+time.Minute), a.agentArchivableCmd(), assign, idle, remove, list)
	return c
}

// registration is what registerAgent did: the entries of other sessions it
// replaced and the open task the new entry holds, empty when none had one;
// own when the task is the session's own (a start registers its session
// busy, and the session's own register keeps it).
type registration struct {
	replaced   []state.Agent
	task       string
	assignedAt time.Time
	park       *state.Park
	own        bool
}

// registerAgent puts me on the roster and says what it replaced. A name is
// one agent's: me replaces its own entry and the entries under its name of
// sessions that no longer run, and is refused a name the entry of a running
// session holds. An open task of a dropped entry is never lost: the new
// entry takes it over with its assignment time, and two dropped entries with
// open tasks are refused, since one entry holds one task.
func registerAgent(st *state.State, me state.Party, live func(state.Party) bool, now time.Time) (registration, error) {
	var reg registration
	var holder, lastTask string
	var keep *state.Keep
	for _, x := range st.Agents {
		own := x.Is(me)
		if !own && !strings.EqualFold(x.Name, me.Name) {
			continue
		}
		if x.LastTask != "" && (own || lastTask == "") {
			lastTask = x.LastTask
		}
		if x.Keep != nil && (own || keep == nil) {
			keep = x.Keep
		}
		if !own {
			if live(x.Party) {
				return registration{}, refused("%q is the name of the running session %s: register under another with --name", x.Name, x.Session)
			}
			reg.replaced = append(reg.replaced, x)
		}
		if x.Task == "" {
			continue
		}
		if reg.task != "" {
			return registration{}, refused("the entries of sessions %s and %s both hold an open task (%q, %q): finish one, or take it off with `beekeeper agents remove`",
				holder, x.Session, reg.task, x.Task)
		}
		reg.task, reg.assignedAt, reg.park, reg.own, holder = x.Task, x.AssignedAt, x.Park, own, x.Session
	}
	st.Agents = slices.DeleteFunc(st.Agents, func(x state.Agent) bool { return x.Is(me) || strings.EqualFold(x.Name, me.Name) })
	st.Agents = append(st.Agents, state.Agent{Party: me, Registered: now, IdleSince: now, Task: reg.task, AssignedAt: reg.assignedAt, Park: reg.park, LastTask: lastTask, Keep: keep})
	return reg, nil
}

// reportIdle ends ag's task and its park. An agent reporting idle again keeps its last
// task and since when it is idle.
// removeAgent takes the agent at i off the roster; its session no longer
// waits on anyone.
func removeAgent(st *state.State, i int) {
	for j := range st.Records {
		if st.Records[j].Session.Is(st.Agents[i].Party) {
			st.Records[j].Waits = ""
		}
	}
	st.Agents = slices.Delete(st.Agents, i, i+1)
}

func reportIdle(ag *state.Agent, now time.Time) {
	ag.Park = nil
	if ag.Task != "" {
		ag.LastTask, ag.Task, ag.IdleSince = ag.Task, "", now.UTC()
	}
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
	t, _ := plat.Machine.Processes() // unreadable: no headless turn is named
	for _, ag := range st.Agents {
		v := agentView{Agent: ag, Reachable: "not running", Browser: agentBrowser(a.cfg, ag), Kept: keptBy(st, ag, a.now)}
		if w := ag.Import; w.Pending(a.now) {
			v.Reachable = "not running, import waits until " + clock(a.now, w.Until)
		}
		if s, ok := claude.Live(sessions, ag.Party); ok {
			v.Reachable, v.Model = "live", s.Model
			for _, c := range s.Commands {
				if c.Remaining > 0 {
					v.Reachable = "waiting, " + dur(c.Remaining) + " left"
				}
			}
			if turn := headlessTurn(t, s.ID); turn != "" {
				v.Reachable, v.HeadlessTurn = "live, "+turn+" running", true
			}
		}
		out = append(out, v)
	}
	slices.SortStableFunc(out, func(x, y agentView) int {
		return boolInt(x.Task != "") - boolInt(y.Task != "")
	})
	return out
}

// stoppedAgents are the agents with a task whose CLI does not run.
func stoppedAgents(agents []state.Agent, sessions []*claude.Session) []state.Agent {
	var out []state.Agent
	for _, ag := range agents {
		if _, live := claude.Live(sessions, ag.Party); ag.Task != "" && !live {
			out = append(out, ag)
		}
	}
	return out
}

// resumeMessage is the turn a resumed worker starts with.
const resumeMessage = "Your CLI stopped (a reboot or a crash). Re-query the live state of your task and continue it."

// resumeHint is how a stopped agent comes back: a desktop session by
// opening its row, a background one by claude --bg --resume.
func resumeHint(ag state.Agent) string {
	if ag.HostSession != "" {
		return "open " + continueURL(ag.HostSession)
	}
	return fmt.Sprintf("claude --bg --resume %s %q", ag.Session, resumeMessage)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (a *app) agentList(full bool) error {
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
	return a.delta("agents", full, a.agentFacts(views), func() { a.printAgents(views) })
}

func (a *app) printAgents(views []agentView) {
	if len(views) == 0 {
		_, _ = fmt.Fprintln(a.out, "no agent is registered")
		return
	}
	w, headless := a.table(), 0
	_, _ = fmt.Fprintln(w, "AGENT\tTASK\tSINCE\tMODEL\tBROWSER\tREACHABLE\tKEPT")
	for _, v := range views {
		task, since := "(idle)", v.IdleSince
		if v.Task != "" {
			task, since = v.Task, v.AssignedAt
		}
		headless += boolInt(v.HeadlessTurn)
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", truncate(v.Name, 30), truncate(task, 60), clock(a.now, since), cmp.Or(truncate(v.Model, 32), "-"), cmp.Or(v.Browser, "-"), v.Reachable, cmp.Or(truncate(v.Kept, 50), "-"))
	}
	_ = w.Flush()
	if headless > 0 {
		_, _ = fmt.Fprintf(a.out, "%s in a headless turn: Claude Desktop's sidebar shows the row idle, this roster is the busy view\n", plural(headless, "agent"))
	}
}
