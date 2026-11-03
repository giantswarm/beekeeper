package cmd

import (
	"context"
	"time"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// A run's scope sits in its build slot's slice, and a slot whose slice
// still runs a scope is taken by no run. beekeeper run stops its scope
// when its command exits; a scope whose run died with it (a killed run, a
// lost go test) runs on with what the command left in it, an envtest
// kube-apiserver say, adopted by the service manager or sandbox init. The
// watch names such an orphaned slot scope at the sample it first sees it and, after
// slotOrphanGrace, stops it: the slot is free without a person.

// slotOrphanGrace is how long the watch leaves an orphaned slot scope
// before it stops it: a run that ends stops its own scope within moments.
const slotOrphanGrace = time.Minute

// slotStopTimeout bounds the stop of an orphaned slot scope.
const slotStopTimeout = 2 * time.Minute

// sandboxInitCommand is bubblewrap, the PID-namespace init of a sandboxed run.
const sandboxInitCommand = "bwrap"

// slotOrphans names each orphaned slot scope once (ORPHANED SLOT SCOPE) and,
// in a watch that does chores, stops it once it has run on for
// slotOrphanGrace (STOPPED, then ENDED); a failed stop is SLOT SCOPE STOP
// FAILED until a stop succeeds.
func (w *watcher) slotOrphans(now time.Time, t *proc.Table) {
	if t == nil {
		return
	}
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
// service manager (or PID 1), or by bubblewrap's PID-namespace init. A live
// run's command is the child of its beekeeper run, outside the scope.
// Bubblewrap inside the scope is part of the command's tree, so its own
// parent must also be checked before the scope can be considered orphaned.
func orphanedScope(t *proc.Table, pids []int) bool {
	in := map[int]bool{}
	for _, pid := range pids {
		in[pid] = true
	}
	adopted := false
	for _, pid := range pids {
		p := t.ByPID[pid]
		var sandboxInit bool
		if p != nil {
			parent := t.ByPID[p.PPID]
			sandboxInit = parent != nil && parent.Comm == sandboxInitCommand
		}
		switch {
		case p == nil, in[p.PPID]:
		case t.Orphaned(p), sandboxInit:
			adopted = true
		default:
			return false
		}
	}
	return adopted
}
