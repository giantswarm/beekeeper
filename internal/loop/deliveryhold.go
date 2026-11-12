package loop

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// The single supervisor writer reconciles at most one board transition per
// pass. A recorded hold survives restarts and retains the prior Status. Note
// holds share that Status: neither releases the other's outstanding blocker.
func (l *Loop) SyncDeliveryHolds(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	st, err := l.Host.Store().Read()
	if err != nil {
		return err
	}
	blocked := map[string]string{}
	for _, d := range st.Deliveries {
		if !d.Active() || slices.Contains(d.Pulls, d.Ref) || d.OutsideDesk || l.unreadRepo(d.Ref) != "" {
			continue
		}
		flow := Cmd.ClassifyDelivery(st, d, l.Host.TimeNow(), l.Host.DeliveryPolicy())
		if flow.Class == deliveryBlocked {
			blocked[strings.ToLower(d.Ref)] = flow.Reason
		}
	}
	for ref, h := range st.DeliveryHolds {
		if _, keep := blocked[ref]; keep {
			continue
		}
		if n, _ := Cmd.HeldBy(st.Notes, ref, 0); n != nil {
			continue
		}
		mv, err := l.Host.BoardClient().Release(ctx, ref, h.Status, h.Was)
		if err != nil {
			return err
		}
		return l.Host.Store().Update(func(current *state.State) ([]state.Event, error) {
			delete(current.DeliveryHolds, ref)
			return []state.Event{Cmd.About(Cmd.Event(LoopParty, "delivery.unblocked", "%s: board Status %s", Cmd.DeliveryURL(ref, false), mv.To), ref)}, nil
		})
	}
	for ref, reason := range blocked {
		if _, held := st.DeliveryHolds[ref]; held {
			continue
		}
		h, err := l.Host.HoldOn(ctx, st.Notes, ref)
		if err != nil {
			return fmt.Errorf("%s: %w", Cmd.DeliveryURL(ref, false), err)
		}
		return l.Host.Store().Update(func(current *state.State) ([]state.Event, error) {
			if current.DeliveryHolds == nil {
				current.DeliveryHolds = map[string]state.ItemHold{}
			}
			current.DeliveryHolds[ref] = *h
			return []state.Event{Cmd.About(Cmd.Event(LoopParty, "delivery.blocked", "%s: %s", Cmd.DeliveryURL(ref, false), reason), ref)}, nil
		})
	}
	return nil
}
