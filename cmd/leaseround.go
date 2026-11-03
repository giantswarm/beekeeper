package cmd

import (
	"fmt"

	"github.com/giantswarm/beekeeper/internal/lease"
)

func (a *app) leaseOverrun(h lease.Holder) string {
	over, round := h.Overrun(a.now, a.cfg.LeaseRound.Duration)
	if over <= 0 {
		return ""
	}
	return fmt.Sprintf("; overrun %s past its %s round", dur(over), dur(round))
}
