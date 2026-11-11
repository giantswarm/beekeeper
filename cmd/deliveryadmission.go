package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	deliveryExemptOnlyFlag = "--wip-exempt-only"
	deliveryNextCommand    = "next"
	deliveryJSONFlag       = "--json"
)

type deliveryWIP struct {
	Limit      int              `json:"limit"`
	Count      int              `json:"count"`
	Items      []state.Delivery `json:"items"`
	CompleteAt time.Time        `json:"complete_at"`
	Reason     string           `json:"reason,omitempty"`
}

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
		serves := loopServes(st, ag)
		if serves != "" && (strings.EqualFold(serves, d.Ref) ||
			slices.ContainsFunc(d.Pulls, func(ref string) bool { return strings.EqualFold(serves, ref) })) ||
			ag.Park != nil && slices.ContainsFunc(d.Pulls, func(ref string) bool { return strings.EqualFold(loopParkOn(ag.Park), ref) }) {
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
func (l *supervisorLoop) deliveryPriority(ctx context.Context, reserved ...[]string) error {
	st, err := l.a.store.Read()
	if err != nil {
		return err
	}
	refs, _ := l.deliveryReservedRefs(st, reserved...)
	if len(refs) == 0 {
		return nil
	}
	if l.deliveryRoom(l.fillRoom(l.fillCapacity(ctx)), st, refs) <= 0 {
		return errLoopLater
	}
	return nil
}

func (l *supervisorLoop) deliveryStart(ref string, exempt bool) error {
	if l.a.cfg.Delivery.WIPLimit == 0 {
		return nil
	}
	st, err := l.a.store.Read()
	if err != nil {
		return err
	}
	if l.dryRun && l.fillPreview != nil {
		st = l.fillPreview
	}
	v := readDeliveryWIP(st, l.a.cfg.Delivery.WIPLimit, l.a.cfg.Supervisor.LoopInterval.Duration, l.a.timeNow(), l.a.deliveryPolicy())
	if v.Reason != "" && !deliveryExempt(st, l.a.timeNow(), ref, exempt) {
		label := deliveryURL(ref, false)
		if label == "" {
			label = "new work"
		}
		l.sayOnce("delivery admission "+ref, "SUPERVISOR NEXT WAITS %s: %s", label, v.Reason)
		return fmt.Errorf("%s: %w", v.Reason, errLoopLater)
	}
	l.clearSaid("delivery admission " + ref)
	return nil
}

func (l *supervisorLoop) deliveryFillRoom(room int, st *state.State, reserved ...[]string) int {
	if l.dryRun && l.fillPreview != nil {
		st = l.fillPreview
	}
	v := readDeliveryWIP(st, l.a.cfg.Delivery.WIPLimit, l.a.cfg.Supervisor.LoopInterval.Duration, l.a.timeNow(), l.a.deliveryPolicy())
	room = l.deliveryRoom(room, st, reserved...)
	if v.Reason != "" {
		// Full intake did no walk. Discard an older empty-board backoff too,
		// so a released place can be used on the very next pass.
		if !l.dryRun {
			l.mu.Lock()
			l.fillPause = fillPause{}
			l.fillEmpty = 0
			l.mu.Unlock()
		}
		if l.a.deliveryExemptIntake(st) {
			return room
		}
		return 0
	}
	if v.Limit > 0 {
		room = min(room, max(0, v.Limit-v.Count))
	}
	return room
}
