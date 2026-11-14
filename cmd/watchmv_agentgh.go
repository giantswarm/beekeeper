package cmd

import (
	"strings"

	"github.com/giantswarm/beekeeper/internal/guard"
)

// ghKey starts the condition key of an agent whose gh is unbrokered.
const ghKey = "gh "

// Unbrokered says each agent on the roster whose shell resolves a gh other
// than the agent's own (agents.shell.path, the gh link to devctl): its gh
// acts on the person's own login, or fails, instead of the App's token. One
// GH UNBROKERED line each, and its ENDED line once the agent's gh is its own
// or it left the roster.
func (w *Watcher) Unbrokered() {
	path := w.Host.cfg.Agents.Shell.Path
	if len(path) == 0 {
		return
	}
	st, err := w.Host.store.Peek()
	if err != nil {
		return
	}
	found := map[string]bool{}
	for _, ag := range st.Agents {
		if ag.GH == "" || guard.Brokered(ag.GH, path) {
			continue
		}
		key := ghKey + ag.Name
		found[key] = true
		w.Emit(key, "GH UNBROKERED %q: its shell resolves gh to %s (read %s), not the agent's own in %s (agents.shell.path): its gh acts without the App's token",
			ag.Name, ag.GH, clock(w.Host.now, ag.GHAt), strings.Join(path, ", "))
	}
	w.ClearMissing(ghKey, found)
}
