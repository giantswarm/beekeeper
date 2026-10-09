package cmd

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// taskOutputExt is the extension of a Claude Code background task's output
// file, <tasks>/<task id>.output, which its command's stdout writes to.
const taskOutputExt = ".output"

// stopTask is the PreToolUse hook's part in a TaskStop of task in session:
// the gate that background task runs (its stdout is the task's output file)
// goes with it. TaskStop sends SIGTERM to the task's process tree, the shell
// first, and kills it after, which the gate cannot tell from its caller's
// session ending, where devctl merges on: so the hook ends the gate's merge
// before the stop does. A waiting merge leaves its lane (a seeded place
// stays, as for a refusal); a running merge's devctl is ended through its
// merge-child, never a pid that is not the merge's recorded merge-child, and
// its gate, or the watch once the gate is gone, records the run, which
// merged nothing and so leaves the lane. It returns what the stopping session
// is told, "" when the task runs no gate.
func (a *app) stopTask(session, task string) string {
	task = strings.TrimSpace(task)
	if task == "" || strings.ContainsRune(task, filepath.Separator) {
		return ""
	}
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return ""
	}
	mine := func(m state.Merge) bool {
		return m.Output != "" && filepath.Base(m.Output) == task+taskOutputExt && (m.By.Session == "" || m.By.Session == session)
	}
	var said []string
	var children []int
	err = store.Update(func(st *state.State) ([]state.Event, error) {
		var evs []state.Event
		me := state.Party{Session: session}
		if i := slices.IndexFunc(st.Merges, func(m state.Merge) bool { return mine(m) && m.By.Session != "" }); i >= 0 {
			me = st.Merges[i].By
		}
		st.Merges = slices.DeleteFunc(st.Merges, func(m state.Merge) bool {
			if !mine(m) {
				return false
			}
			switch m.Phase {
			case state.Waiting:
				what := "leaves lane " + m.Lane
				if m.Seeded {
					what = "keeps its seeded place in lane " + m.Lane
				}
				said = append(said, m.Key()+" "+what)
				evs = append(evs, event(me, "merge.stopped", "%s: its gate (pid %d) is stopped (TaskStop %s), it %s", m.Key(), m.PID, task, what))
				return !m.Seeded
			case state.Running:
				if m.Child == 0 || !proc.Alive(m.Child) || readPID(mergeBase(store.Dir(), m.Repo, m.PR)) != m.Child {
					return false
				}
				children = append(children, m.Child)
				said = append(said, fmt.Sprintf("%s's devctl (merge-child pid %d) is ended; nothing merged, it leaves lane %s", m.Key(), m.Child, m.Lane))
				evs = append(evs, event(me, "merge.stopped", "%s in lane %s: its gate (pid %d) is stopped (TaskStop %s), its devctl (merge-child pid %d) ended", m.Key(), m.Lane, m.PID, task, m.Child))
			}
			return false
		})
		for i := range st.Merges {
			if mine(st.Merges[i]) && st.Merges[i].Phase == state.Waiting {
				st.Merges[i].Output = "" // a seeded place outlives its stopped gate
			}
		}
		return evs, nil
	})
	if err != nil || len(said) == 0 {
		return ""
	}
	for _, pid := range children {
		_ = endChild(pid)
	}
	return GatePrefix + "TaskStop " + task + " stops its gate: " + strings.Join(said, "; ")
}
