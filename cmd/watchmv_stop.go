package cmd

import (
	"github.com/giantswarm/beekeeper/internal/state"
)

// SayStop says the standing stop once, with its reason, and ENDED once
// beekeeper resume lifted it.
func (w *Watcher) SayStop(st *state.State) {
	if st.Stop == nil {
		w.Clear("stop")
		return
	}
	w.Check("stop", true, "STOPPED: %s; the sessions it ended: %s", w.Host.StopText(st.Stop), Cmd.ListOrNone(st.Stop.Sessions))
}
