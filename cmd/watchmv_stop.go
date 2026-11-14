package cmd

import (
	"github.com/giantswarm/beekeeper/internal/state"
)

// sayStop says the standing stop once, with its reason, and ENDED once
// beekeeper resume lifted it.
func (w *watcher) sayStop(st *state.State) {
	if st.Stop == nil {
		w.clear("stop")
		return
	}
	w.check("stop", true, "STOPPED: %s; the sessions it ended: %s", w.a.stopText(st.Stop), listOrNone(st.Stop.Sessions))
}
