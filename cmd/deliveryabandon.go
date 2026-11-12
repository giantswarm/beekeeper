package cmd

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
)

func endedExecution(r *state.ExecutionRecord) bool {
	return r != nil && (r.Completed || r.Terminal() || r.Retirement.Finished())
}

// Positive local evidence is required: missing workers alone can be a partial
// observation. An ended serve record or worker and no claim end this lifecycle.
func abandonEndedDeliveries(st *state.State, now time.Time) []state.Event {
	var events []state.Event
	for i := range st.Deliveries {
		d := &st.Deliveries[i]
		if !d.Active() || d.Agent == "" ||
			d.Step == state.DeliveryReleasing && (d.Verdict == state.DeliveryReleaseFailed || !d.Since.IsZero() && now.Sub(d.Since) < 30*time.Minute) ||
			slices.ContainsFunc(st.Claims, func(c state.Claim) bool { return deliveryMatches(*d, c.Issue) }) ||
			slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return deliveryAgent(st, *d, ag) }) ||
			slices.ContainsFunc(st.Parks, func(p state.Park) bool { return deliveryMatches(*d, p.On) || strings.EqualFold(p.By.Name, d.Agent) }) ||
			slices.ContainsFunc(st.ReviewParks, func(rp state.ReviewPark) bool {
				return deliveryMatches(*d, rp.On) || rp.Serves != "" && deliveryMatches(*d, rp.Serves) || rp.Serves == "" && strings.EqualFold(rp.Agent, d.Agent)
			}) ||
			slices.ContainsFunc(st.Merges, func(m state.Merge) bool {
				return deliveryMatches(*d, m.Repo+"#"+strconv.Itoa(m.PR)) || strings.EqualFold(m.By.Name, d.Agent)
			}) ||
			slices.ContainsFunc(st.Starts, func(s state.Start) bool {
				return strings.EqualFold(s.Name, d.Agent) && s.Execution != nil && !endedExecution(s.Execution)
			}) {
			continue
		}
		live := slices.ContainsFunc(st.Records, func(r state.Record) bool {
			return (deliveryMatches(*d, r.Issue) || strings.EqualFold(r.Session.Name, d.Agent)) && r.Ended.IsZero()
		})
		ended := slices.ContainsFunc(st.Agents, func(ag state.Agent) bool {
			return strings.EqualFold(ag.Name, d.Agent) && ag.Done
		}) || slices.ContainsFunc(st.Records, func(r state.Record) bool {
			return strings.EqualFold(r.Session.Name, d.Agent) && !r.Ended.IsZero()
		}) || slices.ContainsFunc(st.EndedWorkers, func(s state.Start) bool {
			return strings.EqualFold(s.Name, d.Agent)
		}) || slices.ContainsFunc(st.Starts, func(s state.Start) bool {
			return strings.EqualFold(s.Name, d.Agent) && endedExecution(s.Execution)
		}) || slices.ContainsFunc(st.Archives, func(a state.Archive) bool {
			return strings.EqualFold(a.Name, d.Agent)
		})
		if !ended || live {
			continue
		}
		d.Step, d.Since, d.Abandoned, d.Waits = state.DeliveryDropped, now, true, ""
		d.Outcome, d.Evidence, d.OutcomeAt = state.DeliveryDropped, "worker ended without completion outcome", now
		events = append(events, state.DeliveryEvents(nil, []state.Delivery{*d}, loop.LoopParty)...)
	}
	return events
}

// Do not retire an unknown name, role alias, or a worker still on the roster:
// those can recover. Only a positively ended, unregistered target is obsolete.
func endedTimerTarget(st *state.State, target string) bool {
	if target == "" || strings.EqualFold(target, guideRoleAlias) {
		return false
	}
	if resolved, err := continuationTarget(st, target); err == nil && resolved != "" {
		target = resolved
	}
	matches := func(p state.Party) bool {
		return strings.EqualFold(p.Name, target) || p.Session == target || p.HostSession == target
	}
	for _, rl := range roles {
		if h := rl.get(st).Holder; h != nil && matches(h.Party) {
			return false
		}
	}
	if slices.ContainsFunc(st.Parks, func(p state.Park) bool { return matches(p.By) }) ||
		slices.ContainsFunc(st.Records, func(r state.Record) bool { return matches(r.Session) && r.Ended.IsZero() }) ||
		slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return matches(ag.Party) && !ag.Done }) {
		return false
	}
	if i, err := findAgent(st, target); err == nil {
		return st.Agents[i].Done
	}
	if slices.ContainsFunc(st.Starts, func(s state.Start) bool {
		return (matches(s.Party) || s.Execution != nil && string(s.Execution.Request.Ref.Execution) == target) && s.Execution != nil && !endedExecution(s.Execution)
	}) {
		return false
	}
	return slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return matches(ag.Party) && ag.Done }) ||
		slices.ContainsFunc(st.Records, func(r state.Record) bool { return matches(r.Session) && !r.Ended.IsZero() }) ||
		slices.ContainsFunc(st.EndedWorkers, func(s state.Start) bool { return matches(s.Party) }) ||
		slices.ContainsFunc(st.Starts, func(s state.Start) bool {
			return (matches(s.Party) || s.Execution != nil && string(s.Execution.Request.Ref.Execution) == target) && endedExecution(s.Execution)
		}) ||
		slices.ContainsFunc(st.Archives, func(a state.Archive) bool { return matches(a.Party) })
}
