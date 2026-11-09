package cmd

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
func (l *supervisorLoop) syncDeliveryHolds(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	st, err := l.a.store.Read()
	if err != nil {
		return err
	}
	blocked := map[string]string{}
	for _, d := range st.Deliveries {
		if !d.Active() || slices.Contains(d.Pulls, d.Ref) || d.OutsideDesk || l.unreadRepo(d.Ref) != "" {
			continue
		}
		flow := classifyDelivery(st, d, l.a.timeNow(), l.a.deliveryPolicy())
		if flow.Class == deliveryBlocked {
			blocked[strings.ToLower(d.Ref)] = flow.Reason
		}
	}
	for ref, h := range st.DeliveryHolds {
		if _, keep := blocked[ref]; keep {
			continue
		}
		if n, _ := heldBy(st.Notes, ref, 0); n != nil {
			continue
		}
		mv, err := l.a.boardClient().Release(ctx, ref, h.Status, h.Was)
		if err != nil {
			return err
		}
		return l.a.store.Update(func(current *state.State) ([]state.Event, error) {
			delete(current.DeliveryHolds, ref)
			return []state.Event{about(event(loopParty, "delivery.unblocked", "%s: board Status %s", deliveryURL(ref, false), mv.To), ref)}, nil
		})
	}
	for ref, reason := range blocked {
		if _, held := st.DeliveryHolds[ref]; held {
			continue
		}
		h, err := l.a.holdOn(ctx, st.Notes, ref)
		if err != nil {
			return fmt.Errorf("%s: %w", deliveryURL(ref, false), err)
		}
		return l.a.store.Update(func(current *state.State) ([]state.Event, error) {
			if current.DeliveryHolds == nil {
				current.DeliveryHolds = map[string]state.ItemHold{}
			}
			current.DeliveryHolds[ref] = *h
			return []state.Event{about(event(loopParty, "delivery.blocked", "%s: %s", deliveryURL(ref, false), reason), ref)}, nil
		})
	}
	return nil
}
