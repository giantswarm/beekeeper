package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

const deliveryNextCommand = "next"

const deliveryJSONFlag = "--json"

type deliveryWIP struct {
	Limit      int              `json:"limit"`
	Count      int              `json:"count"`
	Items      []state.Delivery `json:"items"`
	CompleteAt time.Time        `json:"complete_at"`
	Reason     string           `json:"reason,omitempty"`
}

// deliveryPriority runs in the command, which holds what it needs (loopDeliveryPriority).
func (l *supervisorLoop) deliveryPriority(ctx context.Context, reserved ...[]string) error {
	return loopDeliveryPriority(l, ctx, reserved...)
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
