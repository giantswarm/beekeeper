package cmd

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

func applyDeliveryVerdict(d *state.Delivery, verdict, tag string, now time.Time) {
	d.Verdict, d.Release = verdict, tag
	if d.Outcome == "" || d.Outcome == state.OutcomeParked {
		d.Outcome, d.OutcomeAt = state.OutcomeMerged, now
	}
	if d.Evidence == "" {
		if len(d.Pulls) > 0 {
			d.Evidence = deliveryURL(d.Pulls[0], true)
		} else if d.Ref != "" {
			d.Evidence = deliveryURL(d.Ref, true)
		}
	}
	if verdict == verdictAvailable || verdict == state.DeliveryNoRelease {
		if d.Step != state.DeliveryDone {
			d.Since = now
		}
		d.Step = state.DeliveryDone
	} else {
		if d.Step != state.DeliveryReleasing {
			d.Since = now
		}
		d.Step = state.DeliveryReleasing
	}
}

// recordDeliveryRelease is called before the lane can forget its merge.
func recordDeliveryRelease(st *state.State, repo string, pr int, out merge.Outcome, handCut string, now time.Time) []state.Event {
	if !out.Merged || pr == 0 {
		return nil
	}
	ref := fmt.Sprintf("%s#%d", repo, pr)
	prev := slices.Clone(st.Deliveries)
	verdict := verdictUnconfirmed
	switch {
	case out.ReleaseFailed:
		verdict = state.DeliveryReleaseFailed
	case out.Unconfirmed:
	case out.NoRelease || handCut != "":
		verdict = state.DeliveryNoRelease
	case out.Release != "":
		verdict = verdictAvailable
	}
	for i := range st.Deliveries {
		d := &st.Deliveries[i]
		if slices.Contains(d.Pulls, ref) || strings.EqualFold(d.Ref, ref) {
			if d.Evidence == "" || d.Outcome == state.OutcomeParked {
				d.Evidence = deliveryURL(ref, true)
			}
			applyDeliveryVerdict(d, verdict, out.Release, now)
		}
	}
	return state.DeliveryEvents(prev, st.Deliveries, LoopParty)
}
