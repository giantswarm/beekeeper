package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// replyAddress is the name a message from by is answered by, which outlives
// the turn that sent it: the role by holds ("guide"), else its roster name;
// "" for a sender that is neither. A message relayed from the command line
// comes from a headless relay turn whose peer socket ends with it, so its
// recipient has nothing else to reply to.
func replyAddress(st *state.State, by state.Party) string {
	for _, rl := range roles {
		if h := rl.get(st).Holder; h != nil && h.Is(by) {
			return rl.name
		}
	}
	if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(by) }); i >= 0 {
		return st.Agents[i].Name
	}
	return ""
}

// fromLine heads msg with the address its sender is answered by; msg as it
// is when there is none or it is the recipient's own.
func fromLine(from, to, msg string) string {
	if from == "" || strings.EqualFold(from, to) {
		return msg
	}
	return fmt.Sprintf("[from %q: reply by SendMessage to %q, or beekeeper agents wake %q]\n%s", from, from, from, msg)
}

// inboxPeer is the PreToolUse hook's delivery of a SendMessage by name to a
// roster agent whose harness reads an inbox, or a live Claude session whose
// instruction is consumed by a boundary hook. It returns the delivery status
// and route, or "" when the target is unknown and the send should pass.
func (a *app) inboxPeer(session, name, msg string) (string, error) {
	if a.loadConfig() != nil {
		return "", nil
	}
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return "", nil
	}
	st, err := store.Read()
	if err != nil {
		return "", nil
	}
	ag, h, id, ok := inboxAgent(st, name)
	if !ok {
		target, err := a.instructionTarget(name)
		if err != nil || target == nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		status, err := a.relayInstruction(ctx, target.ID, instructionAddress(target), msg)
		if err != nil {
			return "", err
		}
		return status + "; instruction reaches the session chat at its next tool or turn boundary", nil
	}
	by := state.Party{Session: session, Name: session}
	for _, rl := range roles {
		if hd := rl.get(st).Holder; hd != nil && hd.Session == session {
			by = hd.Party
		}
	}
	if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return session != "" && ag.Session == session }); i >= 0 {
		by = st.Agents[i].Party
	}
	if err := h.Send(a, ag.Name, id, fromLine(replyAddress(st, by), ag.Name, msg)); err != nil {
		return "", err
	}
	_ = store.Record(event(by, "agents.wake", "%s: SendMessage by name written to its %s inbox", ag.Name, h.Name()))
	return fmt.Sprintf("written to the %s inbox of %s, which runs it at its next tool call or as its next turn", h.Name(), ag.Name), nil
}

// inboxAgent is the local roster agent name addresses (its name, any case,
// or its session id) whose harness reads an inbox, with that harness and
// its start id.
func inboxAgent(st *state.State, name string) (state.Agent, Harness, string, bool) {
	name = strings.TrimSpace(name)
	for _, ag := range st.Agents {
		if ag.Execution != nil || name == "" || !strings.EqualFold(ag.Name, name) && ag.HostSession != name {
			continue
		}
		if h, id, ok := harnessForHost(ag.HostSession); ok {
			return ag, h, id, true
		}
	}
	return state.Agent{}, nil, "", false
}
