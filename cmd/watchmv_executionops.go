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

// syncExecutions reconciles each host on its own schedule and backs off
// hosts whose reconciliation failed or hit its deadline. Rotating the first
// host prevents an unreachable endpoint monopolizing reconciliation.
func (w *watcher) syncExecutions(ctx context.Context, st *state.State) *state.State {
	st = w.retireEndedExecutions(ctx, st)
	selectors := []string{}
	active := map[string]bool{}
	needsSync := map[string]bool{}
	startsByHost := map[string]int{}
	for _, s := range st.Starts {
		if r := s.Execution; r != nil {
			startsByHost[r.Selector]++
			if !r.HostJournalMissing && (executionNeedsSync(st, r) || executionTranscriptPending(r) || w.a.hasUnrecoveredPublication(r)) {
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
	if execution.ValidIdentity(w.a.cfg.Identity.Host) {
		for selector := range w.a.cfg.Agents.Hosts {
			if needsSync[selector] {
				continue
			}
			h, err := w.a.executionHost(selector)
			if err != nil || !hostUsed(st, selector, h.Identity) {
				continue
			}
			if _, ok := w.a.executionClient(h).(execution.InventoryReader); !ok {
				continue
			}
			w.mu.Lock()
			prev, synced := w.executionInventorySynced[selector]
			w.mu.Unlock()
			if synced && startsByHost[selector] >= prev {
				continue
			}
			selectors = append(selectors, selector)
			active[executionSyncEmitPrefix+selector] = true
		}
	}
	w.clearMissing(executionSyncEmitPrefix, active)
	if len(selectors) == 0 {
		return st
	}
	slices.Sort(selectors)
	now := w.a.now
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
		if w.executionInventorySynced == nil {
			w.executionInventorySynced = map[string]int{}
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
		if err := w.a.syncExecutionHost(ctx, selector); err != nil {
			w.mu.Lock()
			w.executionSyncFailures[selector]++
			w.mu.Unlock()
			w.emit(executionSyncEmitPrefix+selector, "EXECUTION UNCONFIRMED: %s: %v", selector, err)
		} else {
			w.mu.Lock()
			delete(w.executionSyncFailures, selector)
			w.executionInventorySynced[selector] = startsByHost[selector]
			w.mu.Unlock()
			w.clear(executionSyncEmitPrefix + selector)
		}
	}
	if !ran {
		return st
	}
	updated, err := w.a.store.Read()
	if err != nil {
		return st
	}
	return w.retireEndedExecutions(ctx, updated)
}
