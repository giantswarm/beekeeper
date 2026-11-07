package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

type deliveryWIP struct {
	Limit      int              `json:"limit"`
	Count      int              `json:"count"`
	Items      []state.Delivery `json:"items"`
	CompleteAt time.Time        `json:"complete_at"`
	Reason     string           `json:"reason,omitempty"`
}

func readDeliveryWIP(st *state.State, limit int, interval time.Duration, now time.Time) *deliveryWIP {
	v := &deliveryWIP{Limit: limit, Items: []state.Delivery{}, CompleteAt: st.DeliveriesAt}
	for _, d := range st.Deliveries {
		if d.Active() {
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
	case st.DeliveriesAt.IsZero(), st.DeliveriesAt.After(now), now.Sub(st.DeliveriesAt) > 3*interval:
		v.Reason = "wip stale: no complete delivery observation within three loop intervals"
	case v.Count >= limit:
		v.Reason = fmt.Sprintf("wip full: %d of %d in delivery", v.Count, limit)
	}
	return v
}

func deliveryExempt(st *state.State, now time.Time, ref string, exempt bool) bool {
	for _, h := range st.Holds {
		if ref != "" && h.Incident && h.Active(now) && strings.EqualFold(h.Except, ref) {
			return true
		}
	}
	return exempt
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
	v := readDeliveryWIP(st, l.a.cfg.Delivery.WIPLimit, l.a.cfg.Supervisor.LoopInterval.Duration, l.a.timeNow())
	if v.Reason != "" && !deliveryExempt(st, l.a.timeNow(), ref, exempt) {
		label := deliveryURL(ref, false)
		if label == "" {
			label = "new work"
		}
		l.sayOnce("delivery admission "+ref, "SUPERVISOR NEXT WAITS %s: %s", label, v.Reason)
		return errLoopLater
	}
	l.clearSaid("delivery admission " + ref)
	return nil
}

func (l *supervisorLoop) deliveryFillRoom(room int, st *state.State, reserved ...[]string) int {
	if l.dryRun && l.fillPreview != nil {
		st = l.fillPreview
	}
	v := readDeliveryWIP(st, l.a.cfg.Delivery.WIPLimit, l.a.cfg.Supervisor.LoopInterval.Duration, l.a.timeNow())
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
		return 0
	}
	if v.Limit > 0 {
		room = min(room, max(0, v.Limit-v.Count))
	}
	return room
}
