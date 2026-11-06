package cmd

import (
	"cmp"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

// executionCannotContinue says why the ended execution r stays ended, "" when
// its conversation can go on. A stop is someone's decision and a retirement
// took the workspace: neither comes back by a message.
func executionCannotContinue(r *state.ExecutionRecord) string {
	switch {
	case r.Released() || r.HostJournalMissing:
		return "its host holds nothing of it"
	case r.StopRequest != nil:
		return "it was stopped: " + r.StopRequest.Reason
	case r.Observation.StopAcknowledged:
		return "it was stopped"
	case r.Retirement != nil:
		return "it is retired and its workspace moved aside"
	case !r.Observation.Ready:
		return "it never accepted its task"
	case !r.Request.Messaging:
		return "it takes no messages"
	case !execution.CanContinue(r.Request.Harness):
		return "its harness " + r.Request.Harness + " has no way back into a conversation"
	}
	return ""
}

// continueExecution runs the ended execution of ag again, in its own workspace
// and conversation, so that a stored message reaches it. It does nothing while
// the process runs. The record comes back first: the runner asks the
// coordinator before its first turn. A host that refused leaves the record as
// it was; a lost answer leaves it for the next observation to correct.
func (a *app) continueExecution(ctx context.Context, by state.Party, ag state.Agent) error {
	ref := *ag.Execution
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	r, _ := executionRecord(st, ref)
	if r == nil {
		return nil
	}
	if r.Completed && !r.Terminal() {
		// A completed worker ends with its turn: a message stored now fails
		// on the host, so its process has to be seen gone first.
		if _, err := a.inspectExecution(ctx, ref); err != nil {
			return err
		}
		if st, err = a.store.Read(); err != nil {
			return err
		}
		if r, _ = executionRecord(st, ref); r == nil {
			return nil
		}
		if !r.Terminal() {
			return refused("%s is ending its last turn: wake it again once its process exited", ag.Name)
		}
	}
	if !r.Terminal() {
		return nil
	}
	if why := executionCannotContinue(r); why != "" {
		s := state.Start{Party: ag.Party, Conversation: r.Request.ConversationID}
		return goneRefusal("%s: %s, so beekeeper cannot continue its conversation: %s", ag.Name, why, a.transcriptHint(ctx, s))
	}
	_, host, err := a.recordedExecution(ref)
	if err != nil {
		return err
	}
	client, ok := a.executionClient(host).(execution.Continuer)
	if !ok {
		return refused("execution transport cannot continue an ended execution")
	}
	key, err := execution.SigningKey(filepath.Join(a.cfg.StateDir, "execution-coordinator"))
	if err != nil {
		return err
	}
	var before state.ExecutionRecord
	var doneBefore bool
	var taskBefore string
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		if err := a.refuseStopped(st, "continuing an SSH execution"); err != nil {
			return nil, err
		}
		if h := startsHold(st, a.now); h != nil && ag.Done {
			return nil, refused("waking %s refused: it is done and %s", ag.Name, a.startsHeldText(h))
		}
		r, _ := executionRecord(st, ref)
		if r == nil || !r.Terminal() {
			return nil, refused("execution of %s changed while it was continued: wake it again", ag.Name)
		}
		before = *r
		r.Phase, r.Completed, r.Settled, r.TranscriptCollected = execution.Activating, false, false, false
		r.Observation.ProcessState, r.Observation.LastError = execution.Activating, ""
		if i := slices.IndexFunc(st.Agents, func(other state.Agent) bool { return other.Execution != nil && *other.Execution == ref }); i >= 0 {
			other := &st.Agents[i]
			doneBefore, taskBefore = other.Done, other.Task
			other.Done, other.Task = false, cmp.Or(other.Task, other.LastTask)
		}
		return []state.Event{event(by, resumedEvent, "%s: its SSH execution goes on in its own conversation", ag.Name)}, nil
	})
	if err != nil {
		return err
	}
	_, err = client.Continue(ctx, ref, execution.ContinueRequest{Receipt: execution.Sign(r.Request, key, time.Now().UTC())})
	if err == nil {
		return nil
	}
	if isUnreachable(err) {
		a.executionFailure(ref, err)
		return err
	}
	if restoreErr := a.store.Update(func(st *state.State) ([]state.Event, error) {
		r, _ := executionRecord(st, ref)
		if r == nil {
			return nil, nil
		}
		r.Phase, r.Completed, r.Settled, r.TranscriptCollected = before.Phase, before.Completed, before.Settled, before.TranscriptCollected
		r.Observation.ProcessState, r.Observation.LastError = before.Observation.ProcessState, before.Observation.LastError
		if i := slices.IndexFunc(st.Agents, func(other state.Agent) bool { return other.Execution != nil && *other.Execution == ref }); i >= 0 {
			st.Agents[i].Done, st.Agents[i].Task = doneBefore, taskBefore
		}
		return nil, nil
	}); restoreErr != nil {
		return errors.Join(err, restoreErr)
	}
	if errors.Is(err, execution.ErrUnsupported) {
		return refused("%s: its host %s cannot continue it: update the host", ag.Name, ref.Host)
	}
	return refused("%s: its host did not continue it: %v", ag.Name, err)
}

// wakeEndedExecution brings the SSH worker s back by name: on the roster with
// its last task, in its own conversation. Delivery that stays pending keeps it
// there: the host runs it and the next sync settles the receipt.
func (a *app) wakeEndedExecution(ctx context.Context, by state.Party, s state.Start, msg string) error {
	ref := s.Execution.Request.Ref
	task := cmp.Or(s.Task, s.Execution.Request.Task, "back by name, from "+by.Name)
	now := a.now.UTC()
	ag := state.Agent{Party: s.Party, Registered: now, ActivityAt: now, Task: task, AssignedAt: now, Harness: s.Harness, Model: s.Model, Execution: &ref}
	text := endedMessage(task, msg)
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		if h := startsHold(st, a.now); h != nil {
			return nil, refused("waking %s refused: it left the roster and %s", s.Name, a.startsHeldText(h))
		}
		if !ag.Is(by) {
			text = fromLine(replyAddress(st, by), ag.Name, text)
		}
		if !onRoster(st, s.Party) {
			st.Agents = append(st.Agents, ag)
		}
		return []state.Event{event(by, resumedEvent, "%s: back on the roster by name, busy with %q", s.Name, task)}, nil
	})
	if err != nil {
		return err
	}
	err = a.continueExecution(ctx, by, ag)
	if err == nil {
		err = a.enqueueExecutionWake(ctx, by, ag, text, nil)
	}
	var pending *executionDeliveryPending
	if err == nil || errors.As(err, &pending) || isUnreachable(err) {
		return err
	}
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		if i := slices.IndexFunc(st.Agents, func(other state.Agent) bool { return other.Is(s.Party) && other.Task == task }); i >= 0 {
			removeAgent(st, i)
		}
		return nil, nil
	})
	return err
}
