package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// guideStopReason flags actionable work at a turn boundary. Future work has
// an owner and deadline in state and therefore survives the turn and relay.
func guideStopReason(st *state.State, session string, now time.Time) string {
	if st.Guide == nil || st.Guide.Holder == nil || !st.Guide.Holder.Is(state.Party{Session: session}) {
		return ""
	}
	var ids []string
	for _, work := range st.GuideWork {
		if guideWorkBlocks(work, now) {
			ids = append(ids, fmt.Sprint(work.ID))
		}
	}
	if len(ids) == 0 {
		if st.Guide.WorkChecked == nil || !st.Guide.WorkChecked.Is(st.Guide.Holder.Party) {
			return "Before ending the guide turn, record every unfinished task, goal and restoration deadline with `beekeeper guide work add --due <time>`, then run `beekeeper guide work check`. Future work must be owned and timed in state, not left as a list in chat."
		}
		return ""
	}
	return "The guide still owns due work (" + strings.Join(ids, ", ") + "). Read `beekeeper guide work`, carry it out or file it, then close it with `beekeeper guide work done <id> --reason <result>`. Future actions must be recorded with their owner and deadline; a list in chat does not hand work over."
}
