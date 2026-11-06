package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/state"
)

// errGone marks a wake of a session that no longer exists: no transcript to
// resume, no harness process reading its inbox. Nothing retries it.
var errGone = errors.New("its session is gone")

// goneError is a refusal of a wake whose session is gone (errGone).
type goneError struct{ err error }

func (g goneError) Error() string               { return g.err.Error() }
func (g goneError) Unwrap() error               { return g.err }
func (goneError) Is(target error) bool          { return target == errGone }
func goneRefusal(format string, a ...any) error { return goneError{refused(format, a...)} }

// wakeParty is the session a wake of q reaches: the roster agent q names
// (true), else a session off the roster (offRosterParty), so a wait the desk
// settles wakes its waiter whether or not it registered. The error is the
// roster's when q names neither.
func (a *app) wakeParty(st *state.State, q string) (state.Agent, bool, error) {
	i, err := findAgent(st, q)
	if err == nil {
		return st.Agents[i], true, nil
	}
	p, ok := offRosterParty(st, q)
	if !ok {
		return state.Agent{}, false, err
	}
	return state.Agent{Party: p}, false, nil
}

// offRosterParty is the session off the roster q addresses: a role's holder
// by its name or ids, a session parked off the
// roster, or a session by its own id (a CLI session id, a desktop local_ id,
// a harness's omp_/agy_/codex_ id), which the caller of a gated wait or a
// park recorded as its owner.
func offRosterParty(st *state.State, q string) (state.Party, bool) {
	q = strings.TrimSpace(q)
	if q == "" {
		return state.Party{}, false
	}
	for _, rl := range roles {
		if h := rl.get(st).Holder; h != nil && (strings.EqualFold(h.Name, q) || h.Session == q || h.HostSession == q) {
			return h.Party, true
		}
	}
	for _, p := range st.Parks {
		if strings.EqualFold(p.By.Name, q) || p.By.Session == q || p.By.HostSession == q {
			return p.By, true
		}
	}
	for _, r := range st.Records {
		if r.Ended.IsZero() && (strings.EqualFold(r.Session.Name, q) || r.Session.Session == q || r.Session.HostSession == q) {
			return r.Session, true
		}
	}
	if strings.HasPrefix(q, claudeHarness{}.HostPrefix()) {
		return state.Party{HostSession: q, Name: q}, true
	}
	if _, _, ok := harnessForHost(q); ok {
		return state.Party{HostSession: q, Name: q}, true
	}
	if _, err := uuid.Parse(q); err == nil {
		return state.Party{Session: q, Name: q}, true
	}
	return state.Party{}, false
}

// endedWorker is the latest local worker named q exactly that a wake cannot
// reach otherwise: off the roster, parked nowhere, its session no role's. Only
// a command naming it on purpose (agents wake, agents resume) looks here: a
// timer or a settled wait for a finished worker must not bring it back.
func (a *app) endedWorker(st *state.State, q string) (state.Start, bool) {
	if _, _, err := a.wakeParty(st, q); err == nil {
		return state.Start{}, false
	}
	var found state.Start
	for _, s := range slices.Concat(st.Starts, st.EndedWorkers) {
		if s.Execution == nil && strings.EqualFold(s.Name, strings.TrimSpace(q)) && !s.At.Before(found.At) {
			task, conv := cmp.Or(s.Task, found.Task), cmp.Or(s.Conversation, found.Conversation)
			found = s
			found.Task, found.Conversation = task, conv
		}
	}
	return found, found.Name != ""
}

// endedMessage is the turn a worker brought back by name starts with: msg
// after what it was doing.
func endedMessage(task, msg string) string {
	if task == "" {
		return "You left the roster and are back in your own conversation.\n\n" + msg
	}
	return fmt.Sprintf("You left the roster and are back in your own conversation, on your task %q.\n\n%s", task, msg)
}

// wakeEnded brings the ended worker s back by name: on the roster with its
// last task, in its own conversation. A session that cannot continue leaves
// the roster as it was, and the refusal names its transcript.
func (a *app) wakeEnded(ctx context.Context, by state.Party, s state.Start, msg, mode string) error {
	var task string
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		if h := startsHold(st, a.now); h != nil {
			return nil, refused("waking %s refused: it left the roster and %s", s.Name, a.startsHeldText(h))
		}
		if onRoster(st, s.Party) {
			return nil, nil
		}
		task = cmp.Or(s.Task, "back by name, from "+by.Name)
		st.Agents = append(st.Agents, state.Agent{Party: s.Party, Registered: a.now.UTC(), ActivityAt: a.now.UTC(), Task: task, AssignedAt: a.now.UTC(), Harness: s.Harness, Model: s.Model})
		return []state.Event{event(by, resumedEvent, "%s: back on the roster by name, busy with %q", s.Name, task)}, nil
	})
	if err != nil {
		return err
	}
	err = wakeOwner(a, ctx, by, wakeID(s.Party), endedMessage(s.Task, msg), mode)
	if err == nil {
		return nil
	}
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(s.Party) && ag.Task == task }); i >= 0 && task != "" {
			removeAgent(st, i)
		}
		return nil, nil
	})
	if !errors.Is(err, errGone) {
		return err
	}
	return goneRefusal("%s: its %s conversation ended and beekeeper cannot continue it: %s", s.Name, cmp.Or(s.Harness, claudeComm), a.transcriptHint(ctx, s))
}

// transcriptHint says where the conversation of s is stored.
func (a *app) transcriptHint(ctx context.Context, s state.Start) string {
	if st, err := a.store.Read(); err == nil {
		_ = a.syncAllTranscripts(ctx, st, false)
	}
	metas, _ := a.transcriptStore().List()
	for _, id := range []string{s.Conversation, s.Session, s.HostSession, s.Name} {
		for _, m := range metas {
			if id != "" && m.ExactMatch(id) {
				return fmt.Sprintf("read it with `beekeeper transcripts read %s`", m.Key)
			}
		}
	}
	return "the transcript store holds no transcript of it"
}

// wakeID is the id a wake of p resolves p by: its CLI session id, else its
// desktop or harness id.
func wakeID(p state.Party) string {
	// A harness's conversation id can also be a UUID. Off the roster that
	// UUID would be mistaken for a Claude session; its host id names the
	// channel that actually owns the conversation.
	if _, _, ok := harnessForHost(p.HostSession); ok {
		return p.HostSession
	}
	return cmp.Or(p.Session, p.HostSession)
}

// onRoster says whether p is a roster agent.
func onRoster(st *state.State, p state.Party) bool {
	return slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) })
}

// callerAgent is the index of the roster agent the caller me is, -1 when
// none: the entry of its session, else, under --as, the entry registered by
// that name, whatever session registered it. That is how a session of
// another harness or a wrapper script acts as its registered agent without
// a harness's session variables. A managed SSH execution's entry is never
// reached by name: its worker reports through its coordinator.
func (a *app) callerAgent(st *state.State, me state.Party) int {
	if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(me) }); i >= 0 || a.as == "" {
		return i
	}
	// An explicit --as compares names and ownership without session identity.
	return slices.IndexFunc(st.Agents, func(ag state.Agent) bool {
		return ag.Execution == nil && strings.EqualFold(ag.Name, me.Name) && state.Party{Name: me.Name, Person: ag.Person, Host: ag.Host}.Is(state.Party{Name: me.Name, Person: me.Person, Host: me.Host})
	})
}
