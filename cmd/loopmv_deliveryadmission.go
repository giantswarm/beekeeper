package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

const DeliveryNextCommand = "next"

const deliveryJSONFlag = "--json"

type deliveryWIP struct {
	Limit      int              `json:"limit"`
	Count      int              `json:"count"`
	Items      []state.Delivery `json:"items"`
	CompleteAt time.Time        `json:"complete_at"`
	Reason     string           `json:"reason,omitempty"`
}

// deliveryPriority runs in the command, which holds what it needs (loopDeliveryPriority).
func (l *Loop) deliveryPriority(ctx context.Context, reserved ...[]string) error {
	return loopDeliveryPriority(l, ctx, reserved...)
}

func (l *Loop) deliveryStart(ref string, exempt bool) error {
	if l.Host.cfg.Delivery.WIPLimit == 0 {
		return nil
	}
	st, err := l.Host.store.Read()
	if err != nil {
		return err
	}
	if l.DryRun && l.FillPreview != nil {
		st = l.FillPreview
	}
	v := readDeliveryWIP(st, l.Host.cfg.Delivery.WIPLimit, l.Host.cfg.Supervisor.LoopInterval.Duration, l.Host.timeNow(), l.Host.deliveryPolicy())
	if v.Reason != "" && !deliveryExempt(st, l.Host.timeNow(), ref, exempt) {
		label := deliveryURL(ref, false)
		if label == "" {
			label = "new work"
		}
		l.SayOnce("delivery admission "+ref, "SUPERVISOR NEXT WAITS %s: %s", label, v.Reason)
		return fmt.Errorf("%s: %w", v.Reason, ErrLoopLater)
	}
	l.ClearSaid("delivery admission " + ref)
	return nil
}

func (l *Loop) DeliveryFillRoom(room int, st *state.State, reserved ...[]string) int {
	if l.DryRun && l.FillPreview != nil {
		st = l.FillPreview
	}
	v := readDeliveryWIP(st, l.Host.cfg.Delivery.WIPLimit, l.Host.cfg.Supervisor.LoopInterval.Duration, l.Host.timeNow(), l.Host.deliveryPolicy())
	room = l.DeliveryRoom(room, st, reserved...)
	if v.Reason != "" {
		// Full intake did no walk. Discard an older empty-board backoff too,
		// so a released place can be used on the very next pass.
		if !l.DryRun {
			l.Mu.Lock()
			l.FillPause = FillPause{}
			l.FillEmpty = 0
			l.Mu.Unlock()
		}
		if l.Host.deliveryExemptIntake(st) {
			return room
		}
		return 0
	}
	if v.Limit > 0 {
		room = min(room, max(0, v.Limit-v.Count))
	}
	return room
}
