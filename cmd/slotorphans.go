package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A run's scope sits in its build slot's slice, and a slot whose slice
// still runs a scope is taken by no run. beekeeper run stops its scope
// when its command exits; a scope whose run died with it (a killed run, a
// lost go test) runs on with what the command left in it, an envtest
// kube-apiserver say, adopted by the service manager. The watch names such
// an orphaned slot scope at the sample it first sees it and, after
// slotOrphanGrace, stops it: the slot is free without a person.

// slotOrphanGrace is how long the watch leaves an orphaned slot scope
// before it stops it: a run that ends stops its own scope within moments.
const slotOrphanGrace = time.Minute

// slotStopTimeout bounds the stop of an orphaned slot scope.
const slotStopTimeout = 2 * time.Minute

// slotOrphans names each orphaned slot scope once (ORPHANED SLOT SCOPE) and,
// in a watch that does chores, stops it once it has run on for
// slotOrphanGrace (STOPPED, then ENDED); a failed stop is SLOT SCOPE STOP
// FAILED until a stop succeeds.
func (w *watcher) slotOrphans(now time.Time, t *proc.Table) {
	if t == nil {
		return
	}
	w.emptySlotHolders(now, t)
	if w.orphanSince == nil {
		w.orphanSince = map[string]time.Time{}
	}
	dir, n := w.slotView()
	o := guard.Options{SlotDir: dir}
	found := map[string]bool{}
	for i := 1; i <= n; i++ {
		s := plat.Machine.SlotSlice(o.SlotSlice(i))
		if s == nil {
			continue
		}
		for _, scope := range s.Scopes {
			sc := plat.Machine.MemcapScope(scope)
			if sc == nil {
				continue
			}
			pids := plat.Machine.ScopePIDs(sc.Path)
			if !orphanedScope(t, pids) {
				continue
			}
			key := "slotorphan." + scope
			found[key] = true
			since, seen := w.orphanSince[scope]
			if !seen {
				since = now
				w.orphanSince[scope] = now
			}
			procs := guard.ProcList(pids, t)
			w.emit(key, "ORPHANED SLOT SCOPE: slot %d's %s runs on without its run: %s; it keeps slot %d busy, the watch stops it at %s",
				i, scope, procs, i, since.Add(slotOrphanGrace).Local().Format("15:04:05"))
			if !w.slotsOn || now.Sub(since) < slotOrphanGrace {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), slotStopTimeout)
			err := plat.Launcher.Stop(ctx, scope)
			cancel()
			w.check(key+".stop", err != nil, "SLOT SCOPE STOP FAILED: %s of slot %d: %v", scope, i, err)
			if err != nil {
				found[key+".stop"] = true
			} else {
				w.emitNow("slotorphan", "STOPPED orphaned slot scope %s of slot %d: %s; slot %d is free", scope, i, procs, i)
				delete(found, key)
			}
		}
	}
	w.clearMissing("slotorphan.", found)
	for scope := range w.orphanSince {
		if !found["slotorphan."+scope] {
			delete(w.orphanSince, scope)
		}
	}
}

// orphanedScope reports whether a scope's processes outlived the run that
// started it: every one is a child of another in the scope or adopted by the
// service manager (or PID 1). A live run's command is the child of its
// beekeeper run, outside the scope.
func orphanedScope(t *proc.Table, pids []int) bool {
	in := map[int]bool{}
	for _, pid := range pids {
		in[pid] = true
	}
	adopted := false
	for _, pid := range pids {
		p := t.ByPID[pid]
		switch {
		case p == nil, in[p.PPID]:
		case t.Orphaned(p):
			adopted = true
		default:
			return false
		}
	}
	return adopted
}

// stopEmptyHolder uses a stable pidfd and rechecks evidence while the wrapper
// is frozen, so a queued memory/cache waiter or a concurrent start is safe.
var stopEmptyHolder = proc.StopIdle

func (w *watcher) emptySlotHolders(now time.Time, t *proc.Table) {
	reader, ok := plat.Capper.(interface{ ScopePopulated(string) (bool, error) })
	if !ok {
		return
	}
	if w.emptyHolderSince == nil {
		w.emptyHolderSince = map[string]time.Time{}
	}
	dir, n := w.slotView()
	found := map[string]bool{}
	for _, slot := range machine.ReadSlots(dir, n) {
		h, ok := guard.ParseHolder(slot.Holder)
		if slot.Free || !ok || h.PID <= 1 || h.Since.IsZero() {
			continue
		}
		p := t.ByPID[h.PID]
		if p == nil || p.Start.IsZero() || len(t.Children(h.PID)) > 0 {
			continue
		}
		if (h.Started.IsZero() && p.Start.After(h.Since)) || (!h.Started.IsZero() && !h.Started.Equal(p.Start)) {
			continue
		}
		// Older holders have no scope/deadline metadata. A run.start event is
		// proof that they passed the legitimate memory and cache waits.
		scope := h.Scope
		ready := !h.StartDeadline.IsZero() && !now.Before(h.StartDeadline)
		if !ready {
			events, err := w.store.Events(1, func(e state.Event) bool {
				unit := guard.RunScope(e.Detail)
				return e.Verb == guard.VerbStart && !e.At.Before(h.Since) && (strings.HasPrefix(unit, fmt.Sprintf("memcap-%d-", h.PID)) || strings.HasPrefix(unit, fmt.Sprintf("memcap-test-%d-", h.PID)))
			})
			if err == nil && len(events) > 0 {
				scope = strings.TrimSuffix(guard.RunScope(events[0].Detail), ".scope")
				ready = true
			}
		}
		if !ready || scope == "" || now.Sub(h.Since) < slotOrphanGrace {
			continue
		}
		empty := func() bool {
			live, err := reader.ScopePopulated(scope)
			if err != nil || live {
				return false
			}
			s := plat.Machine.SlotSlice(guard.Options{SlotDir: dir}.SlotSlice(slot.N))
			if s != nil && len(s.Scopes) > 0 {
				return false
			}
			path := filepath.Join(dir, strconv.Itoa(slot.N)+".holder")
			raw, err := os.ReadFile(filepath.Clean(path))
			return err == nil && strings.TrimSpace(string(raw)) == slot.Holder
		}
		if !empty() {
			continue
		}
		key := fmt.Sprintf("emptyholder:%d:%d:%d", slot.N, h.PID, p.Start.UnixNano())
		found[key] = true
		since, seen := w.emptyHolderSince[key]
		if !seen {
			since = now
			w.emptyHolderSince[key] = now
		}
		w.emit(key, "EMPTY SLOT HOLDER: slot %d held by %s has no live process in %s.scope; the watch recovers it after the grace", slot.N, h.Who(), scope)
		if !w.slotsOn || now.Sub(since) < slotOrphanGrace {
			continue
		}
		err := stopEmptyHolder(h.PID, p.Start, empty)
		w.check(key+".stop", err != nil, "EMPTY SLOT HOLDER STOP FAILED: slot %d: %v", slot.N, err)
		if err == nil {
			w.emitNow("emptyholder", "STOPPED empty slot holder %s of slot %d; its lock is released by process exit", h.Who(), slot.N)
		} else {
			found[key+".stop"] = true
		}
	}
	w.clearMissing("emptyholder:", found)
	for key := range w.emptyHolderSince {
		if !found[key] {
			delete(w.emptyHolderSince, key)
		}
	}
}
