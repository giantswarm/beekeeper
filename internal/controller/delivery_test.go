package controller

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/inbox"
	"github.com/giantswarm/beekeeper/internal/observe"
)

type ackInbox struct {
	mu    sync.Mutex
	acked []string
}

func (a *ackInbox) Run(context.Context, inbox.Handler) error { return nil }
func (a *ackInbox) Unprocessed(context.Context) (int, error) { return 0, nil }
func (a *ackInbox) Ack(id string)                            { a.mu.Lock(); a.acked = append(a.acked, id); a.mu.Unlock() }
func (a *ackInbox) got() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.acked...)
}

var _ interface {
	inbox.Drainer
	inbox.Acker
} = (*ackInbox)(nil)

// A delivery is acknowledged once every read it asked for ran: not while one is put back for a retry,
// and not at all when its reads ran before its routing ended (it is processed at once then).
func TestHeldDeliveries(t *testing.T) {
	t.Parallel()
	in := &ackInbox{}
	rt := New(Config{Clock: clock.NewFake(time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)), Inbox: in})

	rt.holdUnit("d1", unitRoute)
	rt.holdUnit("d1", "Item/a")
	rt.unitDone("d1", "Item/a")
	if rt.routed("d1") || len(in.got()) != 0 {
		t.Fatalf("d1 held although its reads ran: acked %v", in.got())
	}

	rt.holdUnit("d2", unitRoute)
	rt.holdUnit("d2", "Item/a")
	rt.holdUnit("d2", unitClosing+"giantswarm/klaus#55")
	if !rt.routed("d2") {
		t.Fatal("d2 not held")
	}
	rt.unitsDone([]string{"d2"}, unitClosing+"giantswarm/klaus#55")
	if len(in.got()) != 0 {
		t.Fatalf("acked %v before the Item's read", in.got())
	}

	key := "Item/a"
	rt.pending.Add(key, observe.Pending{ReadBoard: true, Deliveries: []string{"d2"}})
	p := rt.pending.Take(key)
	rt.pending.Add(key, *p) // a retry puts the read back
	rt.tookPending(key, p)
	if len(in.got()) != 0 {
		t.Fatalf("acked %v while the read waits for its retry", in.got())
	}
	p = rt.pending.Take(key)
	rt.tookPending(key, p)
	if !reflect.DeepEqual(in.got(), []string{"d2"}) {
		t.Fatalf("acked %v, want d2", in.got())
	}

	// An object that goes takes its pending reads with it.
	rt.holdUnit("d3", unitRoute)
	rt.holdUnit("d3", "Change/b")
	rt.pending.Add("Change/b", observe.Pending{Reads: observe.FullReads(), Deliveries: []string{"d3"}})
	rt.routed("d3")
	rt.dropPending("Change/b")
	if !reflect.DeepEqual(in.got(), []string{"d2", "d3"}) {
		t.Fatalf("acked %v, want d2, d3", in.got())
	}
}

func TestActionRef(t *testing.T) {
	t.Parallel()
	const ref = "giantswarm/roadmap#101"
	for key, want := range map[string]string{
		ref + "/assign/alice":                   ref,
		ref + "/start-run/":                     ref,
		ref:                                     ref,
		"giantswarm/foo.bar#7/todo/team:bumble": "giantswarm/foo.bar#7",
	} {
		if got := actionRef(key); got != want {
			t.Errorf("actionRef(%q) = %q, want %q", key, got, want)
		}
	}
}
