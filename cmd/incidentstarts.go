package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// repositoryStartReason keeps repository holds in force for board selection
// and dispatch. Lane and global merge holds do not restrict starts.
func repositoryStartReason(st *state.State, now time.Time, ref string) string {
	r, ok := parseRef(ref)
	if !ok {
		return ""
	}
	for _, h := range st.Holds {
		if !h.Active(now) || !strings.EqualFold(h.Target, r.Repo) || (strings.EqualFold(h.Except, ref) || strings.EqualFold(h.Except, r.Repo)) {
			continue
		}
		if !h.Incident {
			return fmt.Sprintf("held: repository %s: %s", h.Target, h.Reason)
		}
		return fmt.Sprintf("held: merge incident in %s: %s; only repair %s may start until its release is proven and the hold lifted", h.Target, h.Reason, h.Except)
	}
	return ""
}

// repositoryStart rereads immediately before dispatch, so a hold set while
// the board or GitHub was being read cannot launch another worker.
func (l *supervisorLoop) repositoryStart(ref string) error {
	st, err := l.a.store.Read()
	if err != nil {
		return err
	}
	if why := repositoryStartReason(st, l.a.timeNow(), ref); why != "" {
		l.sayOnce("incident "+ref, "SUPERVISOR START WAITS %s: %s", ref, why)
		return fmt.Errorf("%s: %w", why, errLoopLater)
	}
	l.clearSaid("incident " + ref)
	return nil
}
