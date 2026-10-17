package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// taskOutputExt is the extension of a Claude Code background task's output
// file, <tasks>/<task id>.output, which its command's stdout writes to.
const taskOutputExt = ".output"

var (
	// stopWait is how long a TaskStop waits for an ended devctl's
	// merge-child to exit before it asks GitHub how the merge stands.
	stopWait = 5 * time.Second
	// stopRead bounds that question.
	stopRead = 15 * time.Second
)

// stopped is a running merge a TaskStop reached: its merge-child, ended
// when end says so.
type stopped struct {
	merge state.Merge
	end   bool
}

// stopTask is the PreToolUse hook's part in a TaskStop of task in session:
// the gate that background task runs (the task's output file is its stdout,
// or its shell's when the gate's output is piped on) goes with it. TaskStop
// sends SIGTERM to the task's process tree, the shell first, and kills it
// after, which the gate cannot tell from its caller's session ending, where
// devctl merges on: so the hook ends the gate's merge before the stop does.
// A waiting merge leaves its lane (a seeded place stays, as for a refusal);
// a running merge's devctl is ended through its merge-child, never a pid
// that is not the merge's recorded merge-child, and its gate, or the watch
// once the gate is gone, records the run. It returns what the stopping
// session is told, as the task's own output goes with the stop: the signal
// and each merge's outcome, cancelled or merged before the stop, as GitHub
// reports it once its devctl is gone; "" when the task runs no gate.
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
	var running []stopped
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
				said = append(said, m.Key()+" cancelled before its turn, nothing merged; it "+what)
				evs = append(evs, event(me, "merge.stopped", "%s: its gate (pid %d) is stopped (TaskStop %s), it %s", m.Key(), m.PID, task, what))
				return !m.Seeded
			case state.Running:
				end := m.Child != 0 && proc.Alive(m.Child) && readPID(mergeBase(store.Dir(), m.Repo, m.PR)) == m.Child
				running = append(running, stopped{merge: m, end: end})
				if end {
					evs = append(evs, event(me, "merge.stopped", "%s in lane %s: its gate (pid %d) is stopped (TaskStop %s), its devctl (merge-child pid %d) ended", m.Key(), m.Lane, m.PID, task, m.Child))
				}
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
	if err != nil || len(said)+len(running) == 0 {
		return ""
	}
	for _, s := range running {
		if s.end {
			_ = endChild(s.merge.Child)
		}
	}
	for _, s := range running {
		said = append(said, stopOutcome(s))
	}
	return GatePrefix + "TaskStop " + task + " sends its gate SIGTERM: " + strings.Join(said, "; ")
}

// stopOutcome says how a running merge a TaskStop reached ended: its ended
// devctl gone (stopWait), whether the pull request merged before the stop
// or the stop cancelled it, as GitHub reports it.
func stopOutcome(s stopped) string {
	m := s.merge
	devctl := fmt.Sprintf("its devctl (merge-child pid %d) is ended", m.Child)
	if !s.end {
		devctl = "its devctl had ended before the stop"
	} else if !waitGone(m.Child, stopWait) {
		devctl = fmt.Sprintf("its devctl (merge-child pid %d) is sent SIGTERM and still runs after %s", m.Child, stopWait)
	}
	ctx, cancel := context.WithTimeout(context.Background(), stopRead)
	defer cancel()
	p, err := pullState(ctx, m.Repo, m.PR)
	switch {
	case err != nil:
		return fmt.Sprintf("%s: %s; whether it merged before the stop is unread (%v): its gate records the run", m.Key(), devctl, err)
	case p.State == github.Merged:
		return fmt.Sprintf("%s merged before the stop (at %s): %s, its release wait with it; its gate records the run", m.Key(), p.MergedAt.UTC().Format(time.RFC3339), devctl)
	default:
		return fmt.Sprintf("%s cancelled, GitHub reports it %s: %s, nothing merged; it leaves lane %s", m.Key(), strings.ToLower(p.State), devctl, m.Lane)
	}
}

// waitGone says whether pid exits within d.
func waitGone(pid int, d time.Duration) bool {
	for end := time.Now().Add(d); proc.Alive(pid); time.Sleep(50 * time.Millisecond) {
		if !time.Now().Before(end) {
			return false
		}
	}
	return true
}
