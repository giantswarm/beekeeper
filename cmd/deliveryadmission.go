package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	deliveryExemptOnlyFlag = "--wip-exempt-only"
)

type deliveryPolicy struct {
	person     string
	desk       superloop.Desk
	stallAfter time.Duration
	reserveFor time.Duration
}

func (a *app) deliveryPolicy() deliveryPolicy {
	return deliveryPolicy{person: a.cfg.Guide.Person, stallAfter: a.cfg.Delivery.StallAfter.Duration, reserveFor: a.cfg.Supervisor.ReserveFor.Duration, desk: superloop.Desk{Orgs: a.cfg.GitHub.AllowedOrgs, Never: a.cfg.Supervisor.Ownership.Never}}
}

// Retained observations remain usable across partial sweeps. Local starts are
// included immediately under the same lock as admission, even during an outage.
func deliveryInFlight(st *state.State, d state.Delivery, now time.Time, p deliveryPolicy) bool {
	return deliveryOwned(st, d, now, p) && classifyDelivery(st, d, now, p).Class == deliveryFlowing
}

func deliveryOwned(st *state.State, d state.Delivery, now time.Time, p deliveryPolicy) bool {
	if epicOwner(st, d.Ref) != nil {
		return false
	}
	if reason, _ := p.desk.Unread(d.Ref); reason != "" {
		return false
	}
	if !d.Active() || d.OutsideDesk {
		return false
	}
	for _, ag := range st.Agents {
		if ag.Done || ag.Task == "" {
			continue
		}
		if d.Agent != "" && strings.EqualFold(ag.Name, d.Agent) {
			return true
		}
		serves := loop.LoopServes(st, ag)
		if serves != "" && (strings.EqualFold(serves, d.Ref) ||
			slices.ContainsFunc(d.Pulls, func(ref string) bool { return strings.EqualFold(serves, ref) })) ||
			ag.Park != nil && slices.ContainsFunc(d.Pulls, func(ref string) bool { return strings.EqualFold(loop.LoopParkOn(ag.Park), ref) }) {
			return true
		}
	}
	if d.Origin == state.DeliveryOriginObserved {
		return false
	}
	after := p.stallAfter
	if after <= 0 {
		after = 24 * time.Hour
	}
	switch d.Step {
	case state.DeliveryReviewQueued, state.DeliveryReviewing, state.DeliveryFixing:
		after = p.reserveFor
		if after <= 0 {
			after = config.DefaultReserveFor
		}
	}
	return !d.Since.IsZero() && !d.Since.After(now) && now.Sub(d.Since) < after
}

func readDeliveryWIP(st *state.State, limit int, interval time.Duration, now time.Time, policies ...deliveryPolicy) *deliveryWIP {
	p := deliveryPolicy{}
	if len(policies) > 0 {
		p = policies[0]
	}
	v := &deliveryWIP{Limit: limit, Items: []state.Delivery{}, CompleteAt: st.DeliveriesAt}
	for _, d := range st.Deliveries {
		if deliveryInFlight(st, d, now, p) {
			v.Items = append(v.Items, d)
		}
	}
	v.Count = len(v.Items)
	if limit == 0 {
		return v
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	switch {
	case st.DeliveriesAt.IsZero(), st.DeliveriesAt.After(now), now.Sub(st.DeliveriesAt) > max(10*time.Minute, 3*interval):
		v.Reason = "wip stale: no complete delivery observation within freshness bound"
	case v.Count >= limit:
		v.Reason = fmt.Sprintf("wip full: %d of %d in delivery", v.Count, limit)
	}
	return v
}

// A repair exception belongs to its incident's repository and lasts only
// while that incident is active. Malformed or lifted holds grant no exception.
func deliveryRepairs(st *state.State, now time.Time) []string {
	var refs []string
	for _, h := range st.Holds {
		r, ok := parseRef(h.Except)
		if h.Incident && h.Active(now) && ok && strings.EqualFold(h.Target, r.Repo) {
			refs = append(refs, refName(r))
		}
	}
	return refs
}

func deliveryExempt(st *state.State, now time.Time, ref string, exempt bool) bool {
	return exempt || ref != "" && slices.ContainsFunc(deliveryRepairs(st, now), func(r string) bool { return strings.EqualFold(r, ref) })
}

func (a *app) deliveryExemptIntake(st *state.State) bool {
	return len(deliveryRepairs(st, a.timeNow())) > 0 || slices.ContainsFunc(a.cfg.Board.Order, func(s config.BoardStep) bool { return s.WIPExempt })
}

// Only new issue starts use this check. The frozen references travel with
// their worker stream, never in a field another pass could overwrite.
func loopDeliveryPriority(l *loop.Loop, ctx context.Context, reserved ...[]string) error {
	st, err := asApp(l.Host).store.Read()
	if err != nil {
		return err
	}
	refs, _ := l.DeliveryReservedRefs(st, reserved...)
	if len(refs) == 0 {
		return nil
	}
	if l.DeliveryRoom(loopFillRoom(l, loopFillCapacity(l, ctx)), st, refs) <= 0 {
		return loop.ErrLoopLater
	}
	return nil
}
