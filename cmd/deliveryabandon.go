package cmd

import (
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// Positive local evidence is required: missing workers alone can be a partial
// observation. An ended serve record or archived worker and no claim end this lifecycle.
func abandonEndedDeliveries(st *state.State, now time.Time) []state.Event {
	var events []state.Event
	for i := range st.Deliveries {
		d := &st.Deliveries[i]
		if !d.Active() || d.Agent == "" || slices.ContainsFunc(st.Claims, func(c state.Claim) bool { return deliveryMatches(*d, c.Issue) }) ||
			slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return deliveryAgent(st, *d, ag) }) {
			continue
		}
		ended := slices.ContainsFunc(st.Records, func(r state.Record) bool {
			return deliveryMatches(*d, r.Issue) && strings.EqualFold(r.Session.Name, d.Agent) && !r.Ended.IsZero() && !r.Ended.Before(d.Started)
		})
		live := slices.ContainsFunc(st.Records, func(r state.Record) bool { return deliveryMatches(*d, r.Issue) && r.Ended.IsZero() })
		ended = ended || slices.ContainsFunc(st.Archives, func(a state.Archive) bool {
			return strings.EqualFold(a.Name, d.Agent) && !a.Since.Before(d.Started)
		})
		if !ended || live {
			continue
		}
		d.Step, d.Since, d.Abandoned, d.Waits = state.DeliveryDropped, now, true, ""
		events = append(events, state.DeliveryEvents(nil, []state.Delivery{*d}, loopParty)...)
	}
	return events
}

// Do not retire an unknown name, role alias, or a worker still on the roster:
// those can recover. Only a positively ended, unregistered target is obsolete.
func endedTimerTarget(st *state.State, target string) bool {
	if target == "" || strings.EqualFold(target, guideRoleAlias) {
		return false
	}
	if _, err := findAgent(st, target); err == nil {
		return false
	}
	matches := func(p state.Party) bool {
		return strings.EqualFold(p.Name, target) || p.Session == target || p.HostSession == target
	}
	if slices.ContainsFunc(st.Records, func(r state.Record) bool { return matches(r.Session) && r.Ended.IsZero() }) {
		return false
	}
	return slices.ContainsFunc(st.Records, func(r state.Record) bool { return matches(r.Session) && !r.Ended.IsZero() }) ||
		slices.ContainsFunc(st.Archives, func(a state.Archive) bool { return matches(a.Party) })
}
