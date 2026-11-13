package loop

import (
	"context"
	"fmt"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

const DeliveryNextCommand = "next"

const deliveryJSONFlag = "--json"

type DeliveryWIP struct {
	Limit      int              `json:"limit"`
	Count      int              `json:"count"`
	Items      []state.Delivery `json:"items"`
	CompleteAt time.Time        `json:"complete_at"`
	Reason     string           `json:"reason,omitempty"`
}

// Only new issue starts use this check. The frozen references travel with
// their worker stream, never in a field another pass could overwrite.
func (l *Loop) deliveryPriority(ctx context.Context, reserved ...[]string) error {
	st, err := l.Host.Store().Read()
	if err != nil {
		return err
	}
	refs, _ := l.DeliveryReservedRefs(st, reserved...)
	if len(refs) == 0 {
		return nil
	}
	if l.DeliveryRoom(l.FillRoom(l.FillCapacity(ctx)), st, refs) <= 0 {
		return ErrLoopLater
	}
	return nil
}

func (l *Loop) deliveryStart(ref string, exempt bool) error {
	if l.Host.Cfg().Delivery.WIPLimit == 0 {
		return nil
	}
	st, err := l.Host.Store().Read()
	if err != nil {
		return err
	}
	if l.DryRun && l.FillPreview != nil {
		st = l.FillPreview
	}
	v := Cmd.ReadDeliveryWIP(st, l.Host.Cfg().Delivery.WIPLimit, l.Host.Cfg().Supervisor.LoopInterval.Duration, l.Host.TimeNow(), l.Host.DeliveryPolicy())
	if v.Reason != "" && !Cmd.DeliveryExempt(st, l.Host.TimeNow(), ref, exempt) {
		label := Cmd.DeliveryURL(ref, false)
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
	v := Cmd.ReadDeliveryWIP(st, l.Host.Cfg().Delivery.WIPLimit, l.Host.Cfg().Supervisor.LoopInterval.Duration, l.Host.TimeNow(), l.Host.DeliveryPolicy())
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
		if l.Host.DeliveryExemptIntake(st) {
			return room
		}
		return 0
	}
	if v.Limit > 0 {
		room = min(room, max(0, v.Limit-v.Count))
	}
	return room
}
