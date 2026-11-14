package cmd

import (
	"context"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

const executionSyncHostPrefix = "execution-host-sync:"

const executionSyncEmitPrefix = "execution-sync-"

func hostUsed(st *state.State, selector string, id execution.HostID) bool {
	if p := st.HostPin(selector); p != nil && p.Identity == id && p.PolicyDigest != "" {
		return true
	}
	for _, s := range st.Starts {
		if s.Execution != nil && (s.Execution.Selector == selector || s.Execution.Handle.Ref.Host == id) {
			return true
		}
	}
	for _, ag := range st.Agents {
		if ag.Host == selector || ag.Host == string(id) || (ag.Execution != nil && ag.Execution.Host == id) {
			return true
		}
	}
	for _, rec := range st.Records {
		if rec.Session.Host == selector || rec.Session.Host == string(id) {
			return true
		}
	}
	return false
}

// SyncExecutions reconciles each host on its own schedule and backs off
// hosts whose reconciliation failed or hit its deadline. Rotating the first
// host prevents an unreachable endpoint monopolizing reconciliation.
func (w *Watcher) SyncExecutions(ctx context.Context, st *state.State) *state.State {
	st = w.RetireEndedExecutions(ctx, st)
	selectors := []string{}
	active := map[string]bool{}
	needsSync := map[string]bool{}
	startsByHost := map[string]int{}
	for _, s := range st.Starts {
		if r := s.Execution; r != nil {
			startsByHost[r.Selector]++
			if !r.HostJournalMissing && (Cmd.ExecutionNeedsSync(st, r) || Cmd.ExecutionTranscriptPending(r) || w.Host.HasUnrecoveredPublication(r)) {
				if !slices.Contains(selectors, r.Selector) {
					selectors = append(selectors, r.Selector)
				}
				active[executionSyncEmitPrefix+r.Selector] = true
				needsSync[r.Selector] = true
			}
		}
	}
	// A host may retain a journal after the last coordinator record goes.
	// Inventory runs once for each used, pinned host when Starts contains no
	// active executions on it.
	if execution.ValidIdentity(w.Host.Cfg().Identity.Host) {
		for selector := range w.Host.Cfg().Agents.Hosts {
			if needsSync[selector] {
				continue
			}
			h, err := w.Host.ExecutionHost(selector)
			if err != nil || !hostUsed(st, selector, h.Identity) {
				continue
			}
			if _, ok := w.Host.ExecutionClient(h).(execution.InventoryReader); !ok {
				continue
			}
			w.Mu.Lock()
			prev, synced := w.executionInventorySynced[selector]
			w.Mu.Unlock()
			if synced && startsByHost[selector] >= prev {
				continue
			}
			selectors = append(selectors, selector)
			active[executionSyncEmitPrefix+selector] = true
		}
	}
	w.ClearMissing(executionSyncEmitPrefix, active)
	if len(selectors) == 0 {
		return st
	}
	slices.Sort(selectors)
	now := w.Host.Now()
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
		w.Mu.Lock()
		if w.Last == nil {
			w.Last = map[string]time.Time{}
		}
		if w.executionSyncFailures == nil {
			w.executionSyncFailures = map[string]int{}
		}
		if w.executionInventorySynced == nil {
			w.executionInventorySynced = map[string]int{}
		}
		last := w.Last[key]
		wait := Cmd.ExecutionSyncBackoff(w.executionSyncFailures[selector])
		if !last.IsZero() && now.Sub(last) < wait {
			w.Mu.Unlock()
			continue
		}
		w.Last[key] = now
		w.Mu.Unlock()
		ran = true
		if err := w.Host.SyncExecutionHost(ctx, selector); err != nil {
			w.Mu.Lock()
			w.executionSyncFailures[selector]++
			w.Mu.Unlock()
			w.Emit(executionSyncEmitPrefix+selector, "EXECUTION UNCONFIRMED: %s: %v", selector, err)
		} else {
			w.Mu.Lock()
			delete(w.executionSyncFailures, selector)
			w.executionInventorySynced[selector] = startsByHost[selector]
			w.Mu.Unlock()
			w.Clear(executionSyncEmitPrefix + selector)
		}
	}
	if !ran {
		return st
	}
	updated, err := w.Host.Store().Read()
	if err != nil {
		return st
	}
	return w.RetireEndedExecutions(ctx, updated)
}
