package cmd

import (
	"github.com/giantswarm/beekeeper/internal/gate"
)

func (w *watcher) staleMerges() {
	st, err := w.a.store.Peek()
	if err != nil {
		return
	}
	found := map[string]bool{}
	for _, m := range st.Merges {
		if why := gate.StaleMerge(st, m); why != "" {
			key := "stale-merge " + m.Key()
			found[key] = true
			if !w.isActive(key) {
				w.emit(key, "MERGE STALE: %s: %s", m.Key(), why)
			}
		}
	}
	w.clearMissing("stale-merge ", found)
}
