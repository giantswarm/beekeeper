package cmd

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
)

// enqueueExecutionWake writes the sender's outbox before any transport call.
// CLI sends get fresh identities; scheduled events and explicit retries supply
// their own event keys. Content equality never identifies a distinct send.
func (a *app) enqueueExecutionWake(ctx context.Context, by state.Party, ag state.Agent, text string, resume *state.Park) error {
	if ag.Execution == nil {
		return refused("worker has no owned execution")
	}
	ref := *ag.Execution
	if text == "" || len(guard.CredentialShapes(text)) > 0 {
		return refused("message is empty or credential-shaped")
	}
	sender := execution.Sender{Session: by.Session, Name: by.Name, Host: by.Host}
	key := a.executionRequestID
	if key == "" {
		key = uuid.NewString()
		if resume != nil {
			key = "resume/" + resume.At.UTC().String()
		}
	}
	var id string
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		if err := a.refuseStopped(st, "waking an SSH execution"); err != nil {
			return nil, err
		}
		r, _ := executionRecord(st, ref)
		if r == nil || r.StopRequest != nil || r.Terminal() {
			return nil, refused("owned execution is stopped or unavailable")
		}
		if !r.Request.Messaging {
			return nil, execution.ErrUnsupported
		}
		for _, m := range r.Messages {
			if m.Key != key {
				continue
			}
			digest := m.TextDigest
			if digest == "" {
				digest = execution.Digest([]byte(m.Envelope.Text))
			}
			if digest != execution.Digest([]byte(text)) || m.Envelope.Sender != sender || (m.Resume == nil) != (resume == nil) {
				return nil, refused("message request ID reused with different content")
			}
			if resume != nil && !m.Resume.At.Equal(resume.At) {
				return nil, refused("message request ID names another park")
			}
			id = m.Envelope.ID
			return nil, nil
		}
		id = uuid.NewSHA1(uuid.NameSpaceOID, []byte(ref.Key()+key)).String()
		m := execution.Envelope{ID: id, Sequence: uint64(len(r.Messages)) + 1, From: ref, To: ref, Sender: sender, Kind: execution.MessageWake, Text: text}
		r.Messages = append(r.Messages, state.ExecutionMessage{Envelope: m, Key: key, TextDigest: execution.Digest([]byte(text)), Stage: executionPending, Resume: resume})
		return []state.Event{event(by, "execution.message-stored", "%s: message %s queued at sequence %d", ag.Name, id, m.Sequence)}, nil
	})
	if err != nil {
		return err
	}
	if err := a.syncExecutionMessages(ctx, ref); err != nil {
		return err
	}
	r, _, err := a.recordedExecution(ref)
	if err != nil {
		return err
	}
	for _, m := range r.Messages {
		if m.Envelope.ID == id {
			if m.Stage == execution.Accepted {
				return nil
			}
			if m.Stage == executionPending || m.Stage == execution.Stored || m.Stage == execution.Uncertain {
				return &executionDeliveryPending{err: refused("message %s queued; delivery %s; use agents hosts sync to reconcile", id, m.Stage)}
			}
			return refused("message %s is %s without acceptance; use a new request ID for a deliberate new send", id, m.Stage)
		}
	}
	return errors.New("message receipt disappeared")
}

type executionDeliveryPending struct{ err error }

func (e *executionDeliveryPending) Error() string { return e.err.Error() }
func (e *executionDeliveryPending) Unwrap() error { return e.err }

func (a *app) executionMessagesCmd() *cobra.Command {
	c := &cobra.Command{Use: "messages <agent-or-execution>", Short: "Show durable SSH delivery receipts; reconcile with agents hosts sync", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		ref, err := a.executionByName(args[0])
		if err != nil {
			return err
		}
		r, _, err := a.recordedExecution(ref)
		if err != nil {
			return err
		}
		result := []execution.Delivery{}
		for _, m := range r.Messages {
			e := m.Envelope
			e.Text = ""
			result = append(result, execution.Delivery{Envelope: e, Stage: m.Stage, Resolution: m.Resolution})
		}
		return a.printJSON(result)
	}}
	c.AddCommand(a.executionResolveMessageCmd())
	return c
}

func (a *app) syncExecutionMessages(ctx context.Context, ref execution.Ref) error {
	r, h, err := a.recordedExecution(ref)
	if err != nil {
		return err
	}
	if r.Terminal() || r.Completed {
		return nil
	}
	for _, m := range r.Messages {
		if m.Stage == execution.Accepted || m.Stage == execution.Resolved {
			continue
		}
		if m.Stage == execution.Failed {
			return refused("message %s failed; inspect the owned process before recovery", m.Envelope.ID)
		}
		ack, err := a.executionClient(h).Message(ctx, ref, m.Envelope)
		if err != nil {
			a.executionFailure(ref, err)
			return err
		}
		if ack.ID != m.Envelope.ID || ack.Sequence != m.Envelope.Sequence || !slices.Contains([]string{execution.Stored, execution.Uncertain, execution.Accepted, execution.Failed, execution.Resolved}, ack.Stage) {
			return refused("message acknowledgement identity or stage mismatch")
		}
		if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			r, _ := executionRecord(st, ref)
			if r == nil {
				return nil, refused("execution disappeared")
			}
			for i := range r.Messages {
				if r.Messages[i].Envelope.ID != ack.ID {
					continue
				}
				if r.Messages[i].Stage == execution.Accepted || r.Messages[i].Stage == execution.Resolved {
					return nil, nil
				}
				if r.Messages[i].Stage == execution.Uncertain && ack.Stage == execution.Stored {
					return nil, nil
				}
				r.Messages[i].Stage = ack.Stage
				if ack.Stage == execution.Accepted || ack.Stage == execution.Resolved {
					if r.Messages[i].TextDigest == "" {
						r.Messages[i].TextDigest = execution.Digest([]byte(r.Messages[i].Envelope.Text))
					}
					r.Messages[i].Envelope.Text = ""
				}
				if ack.Stage != execution.Accepted {
					return nil, nil
				}
				if p := r.Messages[i].Resume; p != nil {
					for i := range st.Agents {
						ag := &st.Agents[i]
						if ag.Execution != nil && *ag.Execution == ref && ag.Park != nil && ag.Park.At.Equal(p.At) {
							ag.Park = nil
						}
					}
				}
				sender := r.Messages[i].Envelope.Sender
				return []state.Event{event(state.Party{Session: sender.Session, Name: sender.Name, Host: sender.Host}, "agents.wake", "%s: message %s accepted on %s", r.Request.Name, ack.ID, ref.Host)}, nil
			}
			return nil, refused("message disappeared")
		}); err != nil {
			return err
		}
		if ack.Stage != execution.Accepted && ack.Stage != execution.Resolved {
			if ack.Stage == execution.Failed {
				return refused("message %s failed; inspect the owned process before recovery", ack.ID)
			}
			return nil
		}
	}
	return nil
}
