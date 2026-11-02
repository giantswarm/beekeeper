package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
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
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		r, i := executionRecord(st, ref)
		if r == nil {
			return nil, refused("execution registration disappeared")
		}
		r.Observation.Handle = r.Handle
		r.Observation.InTurn = false
		r.Observation.StopRequested = true
		r.Observation.StopAcknowledged = true
		r.Observation.ProcessState = execution.Exited
		r.Observation.Reachability = execution.Reachable
		r.Observation.ReachabilityAt = time.Now().UTC()
		return []state.Event{event(st.Starts[i].By, "execution.stop-acknowledged", "%s: owned process termination confirmed", st.Starts[i].Name)}, nil
	}); err != nil {
		return err
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
			// Show the persisted process evidence and fresh transport failure
			// while retaining the operation's nonzero result.
			if rec, _, readErr := a.recordedExecution(ref); readErr == nil {
				if printErr := a.printJSON(rec.Observation); printErr != nil {
					return printErr
				}
			}
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
	return &cobra.Command{Use: "recover <agent-or-execution>", Short: "Recover the same registration and execution after an interrupted start or an approved host-policy update", Long: `Recover inspects the recorded execution and never starts a replacement for it.
After an interrupted start it activates the same registration. After a host
upgrade re-prepared its policy under a running execution, that execution's
publication, merge and capped runs refuse until recover rebinds it: only to the
host's prepared policy, and only once its digest is the one pinned in the host
catalog. Until then recover exits 3, naming the reachable unit and why its
publication is unavailable.`, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		page, err := a.executionClient(h).Logs(cmd.Context(), ref, execution.LogRequest{After: cursor, Limit: limit})
		if err != nil {
			a.executionFailure(ref, err)
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

const (
	defaultExecutionOperationTimeout = 30 * time.Second
	executionSyncInterval            = time.Minute
	executionSyncHostPrefix          = "execution-host-sync:"
	executionSyncEmitPrefix          = "execution-sync-"
)

var executionSyncBackoffs = []time.Duration{
	2 * time.Minute,
	4 * time.Minute,
	8 * time.Minute,
	15 * time.Minute,
}

func executionSyncBackoff(failures int) time.Duration {
	if failures <= 0 {
		return executionSyncInterval
	}
	return executionSyncBackoffs[min(failures, len(executionSyncBackoffs))-1]
}

func executionOperationTimeout(h execution.Host) time.Duration {
	if d, err := time.ParseDuration(h.OperationTimeout); err == nil && d > 0 {
		return d
	}
	return defaultExecutionOperationTimeout
}

func activeExecution(st *state.State, ref execution.Ref) bool {
	return slices.ContainsFunc(st.Agents, func(ag state.Agent) bool {
		return ag.Execution != nil && *ag.Execution == ref
	})
}

func executionNeedsSync(st *state.State, r *state.ExecutionRecord) bool {
	if r == nil {
		return false
	}
	if !r.Terminal() || r.Observation.Reachability != execution.Reachable {
		return true
	}
	return !r.Settled && !r.Completed && !r.Observation.StopAcknowledged && activeExecution(st, r.Handle.Ref)
}

func isUnreachable(err error) bool {
	var down *execution.UnreachableError
	return errors.As(err, &down)
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
	syncer, ok := a.executionClient(h).(execution.ControlSyncer)
	if !ok {
		return refused("execution transport cannot reconcile its control outbox")
	}
	timeout := executionOperationTimeout(h)
	withTimeout := func(parent context.Context) (context.Context, context.CancelFunc) {
		return context.WithTimeout(parent, timeout)
	}
	var indices []int
	for i, s := range st.Starts {
		if r := s.Execution; r != nil && r.Selector == selector && executionNeedsSync(st, r) {
			indices = append(indices, i)
		}
	}
	// Reconcile unconfirmed executions first so a persistent deadline or
	// transport failure on one execution does not flip healthy peers on the
	// same host between Reachable and Unreachable across retries.
	slices.SortStableFunc(indices, func(i, j int) int {
		ui := st.Starts[i].Execution.Observation.Reachability == execution.Unreachable
		uj := st.Starts[j].Execution.Observation.Reachability == execution.Unreachable
		if ui == uj {
			return 0
		}
		if ui {
			return -1
		}
		return 1
	})
	var failures []string
	for _, idx := range indices {
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err().Error())
			break
		}
		r := st.Starts[idx].Execution
		ref := r.Handle.Ref
		if st.Stop != nil && !r.Terminal() && r.StopRequest == nil {
			opCtx, cancel := withTimeout(ctx)
			stopErr := a.stopExecution(opCtx, ref, st.Stop.Reason)
			cancel()
			if stopErr != nil {
				failures = append(failures, stopErr.Error())
				if isUnreachable(stopErr) || ctx.Err() != nil {
					break
				}
			}
		}
		opCtx, cancel := withTimeout(ctx)
		_, syncErr := syncer.SyncControl(opCtx, ref)
		cancel()
		if syncErr != nil {
			a.executionFailure(ref, syncErr)
			failures = append(failures, syncErr.Error())
			if isUnreachable(syncErr) || ctx.Err() != nil {
				break
			}
		}
		var obs execution.Observation
		if r.StopRequest != nil && !r.Observation.StopAcknowledged {
			opCtx, cancel = withTimeout(ctx)
			err = a.stopExecution(opCtx, ref, r.StopRequest.Reason)
			cancel()
		} else {
			wasTerminal := r.Terminal()
			opCtx, cancel = withTimeout(ctx)
			obs, err = a.inspectExecution(opCtx, ref)
			cancel()
			if err == nil && !wasTerminal && (obs.ProcessState == execution.Exited || obs.ProcessState == execution.Failed) {
				opCtx, cancel = withTimeout(ctx)
				_, postErr := syncer.SyncControl(opCtx, ref)
				cancel()
				if postErr != nil {
					a.executionFailure(ref, postErr)
					err = postErr
				}
			}
		}
		if err != nil {
			failures = append(failures, err.Error())
			if isUnreachable(err) || ctx.Err() != nil {
				break
			}
		} else if syncErr == nil {
			_ = a.store.Update(func(current *state.State) ([]state.Event, error) {
				if rec, _ := executionRecord(current, ref); rec != nil && rec.Terminal() {
					rec.Settled = true
				}
				return nil, nil
			})
		}
		// Delivery failure must never starve controls or process observations.
		// The delivery helper re-reads the fresh terminal/completed state.
		if st.Stop == nil && r.StopRequest == nil {
			opCtx, cancel = withTimeout(ctx)
			msgErr := a.syncExecutionMessages(opCtx, ref)
			cancel()
			if msgErr != nil {
				failures = append(failures, msgErr.Error())
				if isUnreachable(msgErr) || ctx.Err() != nil {
					break
				}
			}
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d execution reconciliation(s) unconfirmed: %s", len(failures), failures[0])
	}
	return nil
}

// syncExecutions reconciles each host on its own schedule and backs off
// hosts whose reconciliation failed or hit its deadline. Rotating the first
// host prevents an unreachable endpoint monopolizing reconciliation.
func (w *watcher) syncExecutions(ctx context.Context, st *state.State) *state.State {
	selectors := []string{}
	active := map[string]bool{}
	for _, s := range st.Starts {
		if r := s.Execution; r != nil && executionNeedsSync(st, r) {
			if !slices.Contains(selectors, r.Selector) {
				selectors = append(selectors, r.Selector)
			}
			active[executionSyncEmitPrefix+r.Selector] = true
		}
	}
	w.clearMissing(executionSyncEmitPrefix, active)
	if len(selectors) == 0 {
		return st
	}
	slices.Sort(selectors)
	now := w.now
	if now.IsZero() {
		now = time.Now()
	}
	start := int((now.Unix() / 60) % int64(len(selectors)))
	ran := false
	for i := range selectors {
		if ctx.Err() != nil {
			break
		}
		selector := selectors[(start+i)%len(selectors)]
		key := executionSyncHostPrefix + selector
		w.mu.Lock()
		if w.last == nil {
			w.last = map[string]time.Time{}
		}
		if w.executionSyncFailures == nil {
			w.executionSyncFailures = map[string]int{}
		}
		last := w.last[key]
		wait := executionSyncBackoff(w.executionSyncFailures[selector])
		if !last.IsZero() && now.Sub(last) < wait {
			w.mu.Unlock()
			continue
		}
		w.last[key] = now
		w.mu.Unlock()
		ran = true
		if err := w.syncExecutionHost(ctx, selector); err != nil {
			w.mu.Lock()
			w.executionSyncFailures[selector]++
			w.mu.Unlock()
			w.emit(executionSyncEmitPrefix+selector, "EXECUTION UNCONFIRMED: %s: %v", selector, err)
		} else {
			w.mu.Lock()
			delete(w.executionSyncFailures, selector)
			w.mu.Unlock()
			w.clear(executionSyncEmitPrefix + selector)
		}
	}
	if !ran {
		return st
	}
	updated, err := w.store.Read()
	if err != nil {
		return st
	}
	return updated
}

// rebindExecutionPolicy moves a confirmed execution onto its host's prepared
// policy after an approved host-policy update: only the digest pinned in the
// coordinator's host catalog is accepted, and the host requires both that it is
// prepared and that the execution is bound to the policy it replaces. Every
// other difference keeps failing closed and says so: the unit runs on, its
// publication, merge and capped runs refuse.
func (a *app) rebindExecutionPolicy(ctx context.Context, host execution.Host, client execution.Transport, name string, ref execution.Ref, o execution.Observation) (execution.Observation, error) {
	if o.PolicyAvailable() {
		return o, nil
	}
	unavailable := fmt.Sprintf("execution %s of %s on %s is reachable (unit %s, process %s), but its publication, merge and capped runs are unavailable under its policy %s: ", ref.Execution, name, ref.Host, o.Handle.Unit, o.ProcessState, o.PolicyDigest)
	switch {
	case o.HostPolicyDigest == "":
		return o, refused("%sthe host policy is unprepared or stale; prepare the matching bundle on the host, pin its digest in the host catalog, then recover again", unavailable)
	case o.HostPolicyDigest != host.PolicyDigest:
		return o, refused("%sthe host's prepared policy %s is not the catalog's approved %s; pin the prepared digest once approved, then recover again", unavailable, o.HostPolicyDigest, host.PolicyDigest)
	}
	rebinder, ok := client.(execution.PolicyRebinder)
	if !ok {
		return o, refused("%sthe execution transport cannot rebind a policy", unavailable)
	}
	from := o.PolicyDigest
	rebound, err := rebinder.RebindPolicy(ctx, ref, execution.PolicyRebind{From: from, To: host.PolicyDigest})
	if err != nil {
		a.executionFailure(ref, err)
		return o, fmt.Errorf("%spolicy rebind failed: %w", unavailable, err)
	}
	if err := a.saveExecutionObservation(ref, rebound); err != nil {
		return o, err
	}
	if !rebound.PolicyAvailable() {
		return rebound, refused("%sthe host did not confirm the rebind", unavailable)
	}
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		_, i := executionRecord(st, ref)
		if i < 0 {
			return nil, refused("execution is not registered")
		}
		return []state.Event{event(st.Starts[i].By, "execution.policy-rebound", "%s: %s -> %s, approved host policy of %s", st.Starts[i].Name, from, rebound.PolicyDigest, ref.Host)}, nil
	})
	return rebound, err
}
