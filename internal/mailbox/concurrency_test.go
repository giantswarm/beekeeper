package mailbox_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/giantswarm/beekeeper/internal/mailbox"
	"github.com/giantswarm/beekeeper/internal/mailbox/mailboxtest"
)

func TestMailboxConcurrentSenderDedupe(t *testing.T) {
	url := mailboxtest.URL(t)
	stores := []*mailbox.Store{open(t, url), open(t, url)}
	const attempts = 12
	type result struct {
		sent mailbox.Sent
		err  error
	}
	start := make(chan struct{})
	results := make(chan result, attempts)
	for i := range attempts {
		go func() {
			<-start
			m := msg(fmt.Sprintf("recipient-%d@example.com", i), ana, "concurrent-sender-id")
			sent, err := stores[i%len(stores)].Send(context.Background(), m, t0)
			results <- result{sent: sent, err: err}
		}()
	}
	close(start)
	var seq int64
	var accepted int
	for range attempts {
		r := <-results
		if r.err != nil {
			t.Errorf("concurrent retry failed: %v", r.err)
			continue
		}
		if seq == 0 {
			seq = r.sent.Seq
		}
		if r.sent.Seq != seq {
			t.Errorf("one sender's ID produced different deliveries: %d and %d", seq, r.sent.Seq)
		}
		if !r.sent.Duplicate {
			accepted++
		}
	}
	if accepted != 1 {
		t.Errorf("accepted %d copies of the sender's ID, want 1", accepted)
	}
	var pending int
	for i := range attempts {
		pending += len(receive(t, stores[0], fmt.Sprintf("recipient-%d@example.com", i), t0))
	}
	if pending != 1 {
		t.Errorf("queued %d deliveries, want 1", pending)
	}
}

func TestMailboxConcurrentCap(t *testing.T) {
	url := mailboxtest.URL(t)
	stores := []*mailbox.Store{open(t, url), open(t, url)}
	for i := range mailbox.Cap - 1 {
		send(t, stores[0], msg(bob, ana, fmt.Sprintf("cap-fill-%d", i)), t0)
	}
	const attempts = 12
	start := make(chan struct{})
	results := make(chan error, attempts)
	for i := range attempts {
		go func() {
			<-start
			m := msg(bob, fmt.Sprintf("sender-%d@example.com", i), "last-place")
			_, err := stores[i%len(stores)].Send(context.Background(), m, t0)
			results <- err
		}()
	}
	close(start)
	var accepted int
	for range attempts {
		err := <-results
		switch {
		case err == nil:
			accepted++
		case !errors.Is(err, mailbox.ErrFull):
			t.Errorf("send to the last place: %v", err)
		}
	}
	if accepted != 1 {
		t.Errorf("accepted %d sends to the last place, want 1", accepted)
	}
	if n, err := stores[0].Pending(context.Background(), bob, t0); err != nil || n != mailbox.Cap {
		t.Fatalf("pending after concurrent admission: %d, %v", n, err)
	}
}

func TestMailboxCanceledSenderWait(t *testing.T) {
	url := mailboxtest.URL(t)
	s := open(t, url)
	ctx := context.Background()
	blocker, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Close(ctx) })
	// Hold this synthetic sender busy on a separate connection. Canceling
	// a queued send must not publish it later when the sender becomes free.
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_lock(hashtext('beekeeper_sender'), hashtext($1))`, ana); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	m := msg(bob, ana, "canceled-sender-wait")
	if _, err := s.Send(waitCtx, m, t0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled send: %v, want deadline exceeded", err)
	}
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_unlock_all()`); err != nil {
		t.Fatal(err)
	}
	if ds := receive(t, s, bob, t0); len(ds) != 0 {
		t.Fatalf("canceled send queued deliveries: %+v", ds)
	}
	if out := send(t, s, m, t0); out.Duplicate {
		t.Fatal("canceled send retained a dedupe record")
	}
}
