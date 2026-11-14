package github

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/forge/github/wire"
)

// Deliveries lists the App's webhook deliveries newer than cursor (the id of the newest delivery
// seen), oldest first, and returns the new cursor. GitHub lists newest first; the client pages back
// until it reaches the cursor. Without a cursor (the first read) it reads only the newest page; an
// empty log then yields the cursor "0", so the next read pages back to it. The log is read with the
// App's JWT (api app).
func (c *Client) Deliveries(ctx context.Context, cursor string) ([]forge.Delivery, string, error) {
	var last int64
	first := cursor == ""
	if !first {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil {
			return nil, cursor, &forge.Error{Kind: forge.Refused, API: forge.APIApp, Operation: forge.OpListWebhookDeliveries,
				Err: fmt.Errorf("invalid delivery cursor %q", cursor)}
		}
		last = n
	}
	newest := last
	var out []forge.Delivery
	path := "/app/hook/deliveries?per_page=100"
	for page := 0; page < maxPages; page++ {
		var ws []wire.HookDelivery
		_, a, err := c.get(ctx, forge.APIApp, forge.OpListWebhookDeliveries, path, "", &ws)
		if err != nil {
			return nil, cursor, err
		}
		reached := false
		for _, w := range ws {
			if w.ID <= last {
				reached = true
				break
			}
			out = append(out, delivery(w))
			newest = max(newest, w.ID)
		}
		next := c.nextPage(a.header)
		if reached || next == "" || first {
			break
		}
		path = next
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, _ := strconv.ParseInt(out[i].ID, 10, 64)
		b, _ := strconv.ParseInt(out[j].ID, 10, 64)
		return a < b
	})
	return out, strconv.FormatInt(newest, 10), nil
}

// delivery converts a log entry; GitHub's delivery ids grow, the forge type carries them as strings.
func delivery(w wire.HookDelivery) forge.Delivery {
	return forge.Delivery{
		ID:          strconv.FormatInt(w.ID, 10),
		GUID:        w.GUID,
		DeliveredAt: w.DeliveredAt,
		Event:       w.Event,
		Action:      w.Action,
		StatusCode:  w.StatusCode,
		Status:      w.Status,
		Redelivery:  w.Redelivery,
	}
}

// Delivery reads one logged delivery with its request payload (apps/get-webhook-delivery).
func (c *Client) Delivery(ctx context.Context, id string) (forge.DeliveryDetail, error) {
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return forge.DeliveryDetail{}, &forge.Error{Kind: forge.Refused, API: forge.APIApp, Operation: forge.OpGetWebhookDelivery,
			Err: fmt.Errorf("invalid delivery id %q", id)}
	}
	var w wire.HookDeliveryDetail
	if _, _, err := c.get(ctx, forge.APIApp, forge.OpGetWebhookDelivery, "/app/hook/deliveries/"+id, "", &w); err != nil {
		return forge.DeliveryDetail{}, err
	}
	return forge.DeliveryDetail{Delivery: delivery(w.HookDelivery), Payload: []byte(w.Request.Payload)}, nil
}
