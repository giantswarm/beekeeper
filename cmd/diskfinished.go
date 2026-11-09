package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/diskprune"
	"github.com/giantswarm/beekeeper/internal/state"
)

// pruneFinishedState reclaims only artifacts with a durable ended owner. Unknown
// ownership and executions with pending recovery remain protected under pressure.
func (a *app) pruneFinishedState(r *diskRun, m *diskMachine) {
	st, err := a.store.Read()
	if err != nil {
		r.Errors = append(r.Errors, err)
		return
	}
	var paths []string
	for _, start := range st.Starts {
		x := start.Execution
		if x == nil || !x.Retirement.Finished() || !a.executionHostRetirable(st, x) {
			continue
		}
		id := string(x.Handle.Ref.Execution)
		if id != "" && filepath.Base(id) == id {
			paths = append(paths, filepath.Join(a.cfg.StateDir, "execution-sources", id))
		}
	}
	for _, root := range []string{"proofs", "aside"} {
		entries, err := os.ReadDir(filepath.Join(a.cfg.StateDir, root))
		if err != nil && !os.IsNotExist(err) {
			r.Errors = append(r.Errors, err)
		}
		for _, entry := range entries {
			_, task, open := holderOwner(entry.Name(), st)
			if task != "" && !open {
				paths = append(paths, filepath.Join(a.cfg.StateDir, root, entry.Name()))
			}
		}
	}
	for _, path := range paths {
		known, ok := rereadDisk(r, m)
		if !ok || !known {
			r.Keep("finished artifacts: process references unknown")
			return
		}
		r.Add(diskprune.Output(path, time.Time{}, m.refs, r.dry))
	}
}

// protectHarnessStores adds explicit conversation paths, since an idle harness
// need not keep its database open. Unknown conversations retain the age grace.
func (a *app) protectHarnessStores(st *state.State, refs []string) []string {
	for _, start := range st.Starts {
		if start.Harness != harnessAgy || start.Conversation == "" {
			continue
		}
		ended := false
		for _, old := range st.EndedWorkers {
			if old.Is(start.Party) {
				ended = true
				break
			}
		}
		if ended {
			continue
		}
		for _, root := range []string{a.cfg.Agy.ConversationsDir, a.cfg.Agy.BrainDir} {
			if root == "" {
				continue
			}
			entries, _ := os.ReadDir(root)
			for _, e := range entries {
				if e.Name() == start.Conversation || strings.HasPrefix(e.Name(), start.Conversation+".") {
					refs = append(refs, filepath.Join(root, e.Name()))
				}
			}
		}
	}
	return refs
}
