package watch

import (
	"github.com/giantswarm/beekeeper/internal/gate"
)

func (w *Watcher) StaleMerges() {
	st, err := w.Host.Store().Peek()
	if err != nil {
		return
	}
	found := map[string]bool{}
	for _, m := range st.Merges {
		if why := gate.StaleMerge(st, m); why != "" {
			key := "stale-merge " + m.Key()
			found[key] = true
			if !w.IsActive(key) {
				w.Emit(key, "MERGE STALE: %s: %s", m.Key(), why)
			}
		}
	}
	w.ClearMissing("stale-merge ", found)
}
