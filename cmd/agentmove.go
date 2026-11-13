package cmd

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
)

// agentMoveEvent records a task marked to stay on the coordinator, or its
// mark lifted.
const agentMoveEvent = "agents.move"

// executionMove is a host worker's ask to continue its task on the
// coordinator.
type executionMove struct {
	Why string `json:"why"`
}

func (a *app) agentMoveCmd() *cobra.Command {
	var ref string
	var clear bool
	c := &cobra.Command{
		Use:   "move <why>",
		Short: "Move a task to the coordinator: its remaining work needs what only this desk's machine has",
		Long: `Marks a task to stay on the coordinator, the desk's own machine: no start of
it is placed on an execution host again (supervisor.hostsFirst), neither the
loop's (its board start, fresh continuation, fixer or resume) nor a hand start
without --host whose brief names it. Use it when the remaining work needs data,
devices or commands only the coordinator has, such as a proof on production
data that lives there.

A worker on an SSH execution host that finds itself on the wrong machine runs
it from its workspace instead of parking: the coordinator marks its task (the
issue it serves and the pull requests it waits on, asked a desk review of or
published) and queues the task for a fresh worker, which the loop's next pass
starts on the coordinator. That worker sees what is published, never the
host's workspace: publish your work first (agents publish), then move and end
your turn; its brief names the branch and pull request you published. A
worker on the coordinator marks its task the same way; it keeps working, and
its later starts stay here.

--ref marks the task owner/repo#n, the desk's own word before any start;
--clear with --ref lifts the mark.`,
		Example: `  beekeeper agents move "the timing proof needs the production projects on the coordinator"
  beekeeper agents move --ref teemow/lichtspiel#871 "the 300-clip proof needs the production projects"
  beekeeper agents move --ref teemow/lichtspiel#871 --clear`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			why := ""
			if len(args) == 1 {
				why = oneLine(args[0])
			}
			if clear && ref == "" {
				return usageErr("--clear lifts the mark of --ref owner/repo#n")
			}
			if !clear && why == "" {
				return usageErr("say why the task needs the coordinator")
			}
			if clear && why != "" {
				return usageErr("--clear takes no reason")
			}
			if ref != "" {
				if a.managedExecutionID() != "" {
					return refused("on an execution host agents move moves your own task; --ref marks one on the coordinator")
				}
				r, ok := parseRef(ref)
				if !ok {
					return usageErr("--ref %s is no issue or pull request (owner/repo#n)", ref)
				}
				return a.markLocal(refName(r), why, clear)
			}
			if a.managedExecutionID() != "" {
				return a.remoteControl(cmd.Context(), execution.ControlMove, executionMove{Why: why})
			}
			return a.moveHere(why)
		},
	}
	c.Flags().StringVar(&ref, "ref", "", "the task to mark, owner/repo#n; default: the calling worker's")
	c.Flags().BoolVar(&clear, "clear", false, "lift the mark of --ref")
	return c
}

// markLocal marks ref to stay on the coordinator, or lifts its mark.
func (a *app) markLocal(ref, why string, clear bool) error {
	me, err := a.caller()
	if err != nil {
		return err
	}
	var line string
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		if clear {
			if !st.UnmarkLocal(ref) {
				return nil, refused("%s carries no mark to stay on the coordinator", ref)
			}
			line = fmt.Sprintf("%s: its starts are placed as any again (supervisor.hostsFirst)", ref)
			return []state.Event{event(me, agentMoveEvent, "%s: mark lifted, its starts are placed as any again", ref)}, nil
		}
		st.MarkLocal(state.LocalTask{Ref: ref, Why: why, By: me, At: a.now.UTC()})
		line = fmt.Sprintf("%s stays on the coordinator: every start of it runs here (%s)", ref, why)
		return []state.Event{event(me, agentMoveEvent, "%s stays on the coordinator: %s", ref, why)}, nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.out, line)
	return err
}

// moveHere marks the task of a worker on the coordinator: it is where its
// task needs to be, and its later starts stay here.
func (a *app) moveHere(why string) error {
	me, err := a.caller()
	if err != nil {
		return err
	}
	var refs []string
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		i := a.callerAgent(st, me)
		if i < 0 || st.Agents[i].Task == "" || st.Agents[i].Done {
			return nil, refused("this session has no task to move: --ref owner/repo#n marks one")
		}
		ag := st.Agents[i]
		if refs = movedTaskRefs(st, ag); len(refs) == 0 {
			return nil, refused("%q names no issue or pull request of its task: --ref owner/repo#n marks one", ag.Name)
		}
		return markMoved(st, me, ag.Name, refs, why, a.now), nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.out, "already on the coordinator: later starts of %s stay here too\n", strings.Join(refs, ", "))
	return err
}

// moveToCoordinator applies a host worker's agents move: its task, the refs
// of its execution's repository, is marked to stay on the coordinator and
// queued for a fresh worker, which the loop's next pass starts there. It
// validates before it changes anything, as a refused control leaves the
// state unchanged.
func (a *app) moveToCoordinator(st *state.State, ag *state.Agent, me state.Party, why string) (string, []state.Event, error) {
	why = oneLine(why)
	if why == "" {
		return "", nil, refused("say why the task needs the coordinator")
	}
	if ag.Task == "" || ag.Done {
		return "", nil, refused("%q has no task to move", ag.Name)
	}
	if ag.Park != nil && ag.Park.Continuing {
		return "", nil, refused("%s: a fresh continuation is starting already", ag.Name)
	}
	if ag.Execution == nil {
		return "", nil, refused("%q runs on no execution host", ag.Name)
	}
	r, _ := executionRecord(st, *ag.Execution)
	if r == nil {
		return "", nil, refused("%q has no execution record", ag.Name)
	}
	// A host's worker moves its own task only: refs of another repository,
	// such as one an earlier park of it named, stay as they are. The pull
	// request it published (agents publish) is its task too, also before
	// any park or desk review names it.
	refs := movedTaskRefs(st, *ag)
	if p := r.Published; p != nil {
		if pr, ok := parseRef(p.URL); ok && !slices.ContainsFunc(refs, func(x string) bool { return strings.EqualFold(x, refName(pr)) }) {
			refs = append(refs, refName(pr))
		}
	}
	refs = slices.DeleteFunc(refs, func(ref string) bool {
		p, _ := parseRef(ref)
		return !strings.EqualFold(p.Repo, r.Request.Repository)
	})
	if len(refs) == 0 {
		return "", nil, refused("%q names no issue or pull request of %s in its task to move", ag.Name, r.Request.Repository)
	}
	message := fmt.Sprintf("%s asked for the coordinator: %s", ag.Name, why)
	// The fresh worker never sees the host's workspace: it is told what the
	// worker published, so it continues that branch and pull request rather
	// than opening a competing one.
	if p := r.Published; p != nil && p.Branch != "" {
		message += ". It published branch " + p.Branch
		if p.Head != "" {
			message += " at " + p.Head
		}
		if p.URL != "" {
			message += ", pull request " + p.URL
		}
		message += ": continue it, never a competing pull request"
	}
	if ag.Park == nil {
		ag.Park = &state.Park{By: me, At: a.now.UTC(), On: refs[0], Waits: "a fresh worker on the coordinator: " + why}
	}
	if !slices.Contains(ag.Park.Continuation, message) {
		ag.Park.Continuation = append(ag.Park.Continuation, message)
	}
	r.Completed = true
	events := append(markMoved(st, me, ag.Name, refs, why, a.now), event(me, continuationQueuedEvent, "%s: %s", ag.Name, message))
	return fmt.Sprintf("moved: a fresh worker continues %s on the coordinator, from what is published, not from this workspace; end your turn now", issueURL(refs[0])), events, nil
}

// markMoved marks refs, a worker's task, to stay on the coordinator.
func markMoved(st *state.State, me state.Party, name string, refs []string, why string, now time.Time) []state.Event {
	for _, ref := range refs {
		st.MarkLocal(state.LocalTask{Ref: ref, Why: why, By: me, At: now.UTC()})
	}
	return []state.Event{event(me, agentMoveEvent, "%s: %s stays on the coordinator: %s", name, strings.Join(refs, ", "), why)}
}

// movedTaskRefs are the issue and pull requests ag's task is, owner/repo#n:
// the issue it serves (its serve record, else the first issue its task
// names) and the pull requests it waits on or asked a desk review of; else
// the reference its task names.
func movedTaskRefs(st *state.State, ag state.Agent) []string {
	var refs []string
	add := func(raw string) {
		r, ok := parseRef(raw)
		if !ok {
			return
		}
		name := refName(r)
		if !slices.ContainsFunc(refs, func(x string) bool { return strings.EqualFold(x, name) }) {
			refs = append(refs, name)
		}
	}
	add(loop.LoopServes(st, ag))
	add(loop.LoopTaskServes(ag, ""))
	add(parkedPull(ag.Park))
	for _, rp := range st.ReviewParks {
		if rp.Party.Is(ag.Party) {
			add(rp.On)
		}
	}
	if len(refs) == 0 {
		add(loop.AgentRef(st, ag.Name))
	}
	return refs
}
