package cmd

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// retaskEvent is the log's verb of a restored task record.
const retaskEvent = "agents.retask"

// retask restores the task record of ag, which had a task and reported it
// idle (agents idle, --done included), from msg, the supervisor's message
// that hands it the next: its task is the message's first line (its last
// task when that is empty), assigned now, and it is no longer done, so
// agents park keeps it and the doctor does not archive it mid-task. An
// agent that is busy, or never had a task, is left alone. It reports
// whether it restored the record.
func retask(ag *state.Agent, msg string, now time.Time) bool {
	if !retaskable(*ag) {
		return false
	}
	ag.Task, ag.AssignedAt, ag.Done = cmp.Or(briefTask(msg), ag.LastTask), now.UTC(), false
	return true
}

// retaskable says whether a message re-tasks ag: it had a task and
// reported it idle.
func retaskable(ag state.Agent) bool { return ag.Task == "" && ag.LastTask != "" }

// retaskAgent restores the task record of the roster agent i (retask) when
// by, who sends it msg, holds the supervisor role. It returns the line the
// sender is told, "" when nothing was restored.
func (a *app) retaskAgent(by state.Party, i func(*state.State) int, msg string) (string, error) {
	var line string
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		if st.Supervisor == nil || !st.Supervisor.Is(by) {
			return nil, nil
		}
		j := i(st)
		if j < 0 {
			return nil, nil
		}
		ag := &st.Agents[j]
		if !retask(ag, msg, a.now) {
			return nil, nil
		}
		line = fmt.Sprintf("%s is busy again with %q: the message re-tasks it, so agents park keeps its task", ag.Name, ag.Task)
		return []state.Event{event(by, retaskEvent, "%s: %s", ag.Name, ag.Task)}, nil
	})
	return line, err
}

// messagedAgent finds the roster agent a SendMessage's target to names: a
// name or session id, or the peer socket (uds:…) of its running CLI.
func messagedAgent(sessions []*claude.Session, to string, sock func(pid int) string) func(*state.State) int {
	return func(st *state.State) int {
		path, isSocket := strings.CutPrefix(to, socketAddress)
		if !isSocket {
			i, err := findAgent(st, to)
			if err != nil {
				return -1
			}
			return i
		}
		k := slices.IndexFunc(sessions, func(s *claude.Session) bool { return s.PID != 0 && sock(s.PID) == path })
		if k < 0 {
			return -1
		}
		return rosterIndex(sessions[k].Party())(st)
	}
}

// rosterIndex finds the roster agent p.
func rosterIndex(p state.Party) func(*state.State) int {
	return func(st *state.State) int {
		return slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) })
	}
}

// socketAddress starts a SendMessage target that is a CLI's peer socket.
const socketAddress = "uds:"
