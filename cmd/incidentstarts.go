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
