package cmd

import (
	"context"
	"fmt"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

func (a *app) executionByName(name string) (execution.Ref, error) {
	st, err := a.store.Read()
	if err != nil {
		return execution.Ref{}, err
	}
	for _, s := range st.Starts {
		if s.Execution != nil && string(s.Execution.Handle.Ref.Execution) == name {
			return s.Execution.Handle.Ref, nil
		}
	}
	i, err := findAgent(st, name)
	if err != nil {
		return execution.Ref{}, err
	}
	if st.Agents[i].Execution == nil {
		return execution.Ref{}, refused("agent has no managed SSH execution")
	}
	return *st.Agents[i].Execution, nil
}

func (a *app) recordedExecution(ref execution.Ref) (*state.ExecutionRecord, execution.Host, error) {
	st, err := a.store.Read()
	if err != nil {
		return nil, execution.Host{}, err
	}
	r, _ := executionRecord(st, ref)
	if r == nil {
		return nil, execution.Host{}, refused("execution is not owned by this coordinator")
	}
	h, err := a.executionHost(r.Selector)
	if err == nil && h.Identity != ref.Host {
		err = refused("recorded host identity changed")
	}
	return r, h, err
}

func (a *app) inspectExecution(ctx context.Context, ref execution.Ref) (execution.Observation, error) {
	_, h, err := a.recordedExecution(ref)
	if err != nil {
		return execution.Observation{}, err
	}
	o, err := a.executionClient(h).Inspect(ctx, ref)
	if err != nil {
		a.executionFailure(ref, err)
		return o, err
	}
	return o, a.saveExecutionObservation(ref, o)
}

func (a *app) stopExecution(ctx context.Context, ref execution.Ref, reason string) error {
	// Intent is durable before contacting the host. An unreachable host never
	// releases a reservation or clears a task.
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		r, i := executionRecord(st, ref)
		if r == nil {
			return nil, refused("execution is not owned")
		}
		if r.StopRequest == nil {
			r.StopRequest = &execution.StopRequest{ID: uuid.NewString(), Reason: reason}
		}
		return []state.Event{event(st.Starts[i].By, "execution.stop-requested", "%s: %s", st.Starts[i].Name, reason)}, nil
	})
	if err != nil {
		return err
	}
	r, h, err := a.recordedExecution(ref)
	if err != nil {
		return err
	}
	ack, err := a.executionClient(h).Stop(ctx, ref, *r.StopRequest)
	if err != nil {
		a.executionFailure(ref, err)
		return err
	}
	if ack.ID != r.StopRequest.ID || ack.Stage != "applied" {
		return refused("termination is unconfirmed")
	}
	_, err = a.inspectExecution(ctx, ref)
	return err
}

func (a *app) executionInspectCmd() *cobra.Command {
	return &cobra.Command{Use: "inspect <agent-or-execution>", Short: "Inspect an owned SSH execution without changing its task", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ref, err := a.executionByName(args[0])
		if err != nil {
			return err
		}
		o, err := a.inspectExecution(cmd.Context(), ref)
		if err != nil {
			return err
		}
		return a.printJSON(o)
	}}
}
func (a *app) executionStopCmd() *cobra.Command {
	var reason string
	c := &cobra.Command{Use: "stop <agent-or-execution>", Short: "Persist stop intent and stop only the owned SSH unit", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ref, err := a.executionByName(args[0])
		if err != nil {
			return err
		}
		return a.stopExecution(cmd.Context(), ref, reason)
	}}
	c.Flags().StringVar(&reason, "reason", "operator stop", "reason recorded before delivery")
	return c
}
func (a *app) executionRecoverCmd() *cobra.Command {
	return &cobra.Command{Use: "recover <agent-or-execution>", Short: "Recover the same registration and execution after an interrupted start", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ref, err := a.executionByName(args[0])
		if err != nil {
			return err
		}
		return a.activateExecution(cmd.Context(), ref)
	}}
}
func (a *app) executionLogsCmd() *cobra.Command {
	var cursor string
	var limit int
	c := &cobra.Command{Use: "logs <agent-or-execution>", Short: "Read a bounded page of an owned SSH execution log", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ref, err := a.executionByName(args[0])
		if err != nil {
			return err
		}
		_, h, err := a.recordedExecution(ref)
		if err != nil {
			return err
		}
		page, err := a.executionClient(h).Logs(cmd.Context(), ref, execution.LogRequest{Cursor: cursor, Limit: limit})
		if err != nil {
			return err
		}
		if a.json {
			return a.printJSON(page)
		}
		_, err = a.out.Write(page.Data)
		return err
	}}
	c.Flags().StringVar(&cursor, "cursor", "", "cursor from the previous JSON page")
	c.Flags().IntVar(&limit, "limit", 64<<10, "maximum bytes in a page")
	return c
}

func (a *app) syncExecutionHost(ctx context.Context, selector string) error {
	h, err := a.executionHost(selector)
	if err != nil {
		return err
	}
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	var failures []string
	for _, s := range st.Starts {
		r := s.Execution
		if r == nil || r.Selector != selector {
			continue
		}
		ref := r.Handle.Ref
		var result execution.ControlResult
		if err := (execution.SSHTransport{Host: h}).Call(ctx, "sync-control", ref, nil, &result); err != nil {
			a.executionFailure(ref, err)
			failures = append(failures, err.Error())
			continue
		}
		if r.StopRequest != nil && !r.Observation.StopAcknowledged {
			err = a.stopExecution(ctx, ref, r.StopRequest.Reason)
		} else {
			_, err = a.inspectExecution(ctx, ref)
		}
		if err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d execution reconciliation(s) unconfirmed: %s", len(failures), failures[0])
	}
	return nil
}
