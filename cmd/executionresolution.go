package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) executionResolveMessageCmd() *cobra.Command {
	var reason string
	c := &cobra.Command{Use: "resolve <agent-or-execution> <message-id>", Short: "Resolve failed or uncertain delivery without replay or an acceptance claim", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		ref, err := a.executionByName(args[0])
		if err != nil {
			return err
		}
		if err := a.resolveExecutionMessage(cmd.Context(), ref, execution.MessageResolution{ID: args[1], Reason: reason}); err != nil {
			return err
		}
		_, err = fmt.Fprintln(a.out, "message resolved without replay; its park remains until an accepted resume")
		return err
	}}
	c.Flags().StringVar(&reason, "reason", "", "audit reason; uncertain delivery may already have been consumed")
	_ = c.MarkFlagRequired("reason")
	return c
}

func (a *app) resolveExecutionMessage(ctx context.Context, ref execution.Ref, q execution.MessageResolution) error {
	if strings.TrimSpace(q.Reason) == "" || len(q.Reason) > 1024 || len(guard.CredentialShapes(q.Reason)) > 0 {
		return refused("resolution requires a bounded, credential-free audit reason")
	}
	_, h, err := a.recordedExecution(ref)
	if err != nil {
		return err
	}
	resolver, ok := a.executionClient(h).(execution.MessageResolver)
	if !ok {
		return refused("execution transport cannot resolve delivery")
	}
	var sequence uint64
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		r, _ := executionRecord(st, ref)
		if r == nil {
			return nil, refused("execution disappeared")
		}
		for i := range r.Messages {
			m := &r.Messages[i]
			if m.Envelope.ID != q.ID {
				continue
			}
			if m.Stage != execution.Failed && m.Stage != execution.Uncertain && m.Stage != execution.Resolved {
				return nil, refused("only failed or uncertain delivery can be resolved")
			}
			if m.Resolution != "" && m.Resolution != q.Reason {
				return nil, refused("resolution reason conflicts with the original request")
			}
			m.Resolution = q.Reason
			sequence = m.Envelope.Sequence
			return []state.Event{event(state.Party{Name: "operator"}, "execution.message-resolution-requested", "%s: message %s: %s", r.Request.Name, q.ID, q.Reason)}, nil
		}
		return nil, refused("message is not registered")
	})
	if err != nil {
		return err
	}
	ack, err := resolver.ResolveMessage(ctx, ref, q)
	if err != nil {
		return err
	}
	if ack.ID != q.ID || ack.Sequence != sequence || ack.Stage != execution.Resolved {
		return refused("resolution acknowledgement mismatch")
	}
	return a.store.Update(func(st *state.State) ([]state.Event, error) {
		r, _ := executionRecord(st, ref)
		if r == nil {
			return nil, refused("execution disappeared")
		}
		for i := range r.Messages {
			m := &r.Messages[i]
			if m.Envelope.ID != q.ID {
				continue
			}
			if m.Stage == execution.Accepted {
				return nil, refused("message already accepted")
			}
			if m.TextDigest == "" {
				m.TextDigest = execution.Digest([]byte(m.Envelope.Text))
			}
			m.Stage, m.Envelope.Text = execution.Resolved, ""
			return []state.Event{event(state.Party{Name: "operator"}, "execution.message-resolved", "%s: message %s skipped without replay: %s", r.Request.Name, q.ID, q.Reason)}, nil
		}
		return nil, refused("message disappeared")
	})
}
