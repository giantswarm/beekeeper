package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// incidentStartReason applies only to repository outage holds. Ordinary
// proving windows and lane holds keep their existing starts behavior.
func incidentStartReason(st *state.State, now time.Time, ref string) string {
	r, ok := parseRef(ref)
	if !ok {
		return ""
	}
	for _, h := range st.Holds {
		if !h.Incident || !h.Active(now) || !strings.EqualFold(h.Target, r.Repo) || strings.EqualFold(h.Except, ref) {
			continue
		}
		return fmt.Sprintf("held: merge incident in %s: %s; only repair %s may start until its release is proven and the hold lifted", h.Target, h.Reason, h.Except)
	}
	return ""
}

// incidentStart rereads immediately before dispatch, so a hold set while
// the board or GitHub was being read cannot launch another worker.
func (l *supervisorLoop) incidentStart(ref string) error {
	st, err := l.a.store.Read()
	if err != nil {
		return err
	}
	if why := incidentStartReason(st, l.a.timeNow(), ref); why != "" {
		l.sayOnce("incident "+ref, "SUPERVISOR START WAITS %s: %s", ref, why)
		return fmt.Errorf("%s: %w", why, errLoopLater)
	}
	l.clearSaid("incident " + ref)
	return nil
}
