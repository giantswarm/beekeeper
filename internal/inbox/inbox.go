// Package inbox is the durable queue between the webhook receiver and the leader (plan §4, §5): the
// receiver stores each verified delivery before answering 2xx; the leader drains it.
package inbox

import (
	"context"
	"errors"
	"time"
)

// Delivery is a stored webhook delivery.
type Delivery struct {
	ID         string // X-GitHub-Delivery
	Event      string // X-GitHub-Event
	Action     string
	ReceivedAt time.Time
	Payload    []byte
}

// Store is the receiver's side: Put stores a delivery once.
type Store interface {
	// Put stores d; duplicate is true when a delivery with the same ID was stored before.
	Put(ctx context.Context, d Delivery) (duplicate bool, err error)
}

// Handler processes one delivery; an error leaves it unprocessed for the next drain.
type Handler func(ctx context.Context, d Delivery) error

// ErrHeld, returned by a Handler, leaves the delivery unprocessed but out of the drain: the handler
// queued what it asked for in memory and acknowledges the delivery (Acker.Ack) once that ran. A drainer
// that starts again (the next leader) hands it out again.
var ErrHeld = errors.New("inbox: delivery held until its reads ran")

// Acker acknowledges deliveries a Handler held: they are marked processed. An Ack may come before the
// Handler that held the delivery has returned.
type Acker interface {
	Ack(id string)
}

// Drainer is the leader's side.
type Drainer interface {
	// Run drains unprocessed deliveries in arrival order until ctx ends, waking on new ones.
	Run(ctx context.Context, h Handler) error
	// Unprocessed is the number of stored deliveries not yet handled (settle probe).
	Unprocessed(ctx context.Context) (int, error)
}
