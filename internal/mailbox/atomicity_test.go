package mailbox_test

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/giantswarm/beekeeper/internal/mailbox"
	"github.com/giantswarm/beekeeper/internal/mailbox/mailboxtest"
)

// notificationFault shadows pg_notify only in this test's database. The
// real notification still runs on success; a fault happens after the queue
// mutation, at the boundary that must share its transaction.
type notificationFault struct {
	control *pgx.Conn
}

func openNotificationFault(t *testing.T) (*mailbox.Store, notificationFault) {
	t.Helper()
	u, err := url.Parse(mailboxtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", "public,pg_catalog")
	u.RawQuery = q.Encode()
	ctx := context.Background()
	control, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close(ctx) })
	_, err = control.Exec(ctx, `
		CREATE TABLE notification_fault (enabled bool NOT NULL);
		INSERT INTO notification_fault VALUES (false);
		CREATE FUNCTION public.pg_notify(text, text) RETURNS void LANGUAGE plpgsql AS $$
		BEGIN
			IF (SELECT enabled FROM notification_fault) THEN
				RAISE EXCEPTION 'synthetic notification failure';
			END IF;
			PERFORM pg_catalog.pg_notify($1, $2);
		END;
		$$;
	`)
	if err != nil {
		t.Fatal(err)
	}
	return open(t, u.String()), notificationFault{control: control}
}

func (f notificationFault) enable(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.control.Exec(ctx, "LISTEN "+mailbox.Channel); err != nil {
		t.Fatal(err)
	}
	if _, err := f.control.Exec(ctx, `UPDATE notification_fault SET enabled = true`); err != nil {
		t.Fatal(err)
	}
}

// recovered checks the operation reached the injected fault and published
// no change. A committed marker follows the failed transaction, so the
// next notification must be that marker, without polling or sleeps.
func (f notificationFault) recovered(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Message != "synthetic notification failure" {
		t.Fatalf("operation error: %v, want the injected notification failure", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := f.control.Exec(ctx, `UPDATE notification_fault SET enabled = false`); err != nil {
		t.Fatal(err)
	}
	const marker = "atomicity-marker"
	if _, err := f.control.Exec(ctx, `SELECT pg_catalog.pg_notify($1, $2)`, mailbox.Channel, marker); err != nil {
		t.Fatal(err)
	}
	n, err := f.control.WaitForNotification(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n.Channel != mailbox.Channel || n.Payload != marker {
		t.Fatalf("failed transaction published a change: %+v", n)
	}
}

func TestMailboxSendNotificationFailure(t *testing.T) {
	s, fault := openNotificationFault(t)
	m := msg(bob, ana, "send-after-notification-failure")
	fault.enable(t)
	_, err := s.Send(context.Background(), m, t0)
	fault.recovered(t, err)
	if ds := receive(t, s, bob, t0); len(ds) != 0 {
		t.Fatalf("failed send retained a delivery: %+v", ds)
	}
	out := send(t, s, m, t0)
	if out.Duplicate {
		t.Fatal("failed send retained a dedupe record")
	}
	if ds := receive(t, s, bob, t0); len(ds) != 1 || ds[0].Seq != out.Seq {
		t.Fatalf("retried send: %+v", ds)
	}
}

func TestMailboxAckNotificationFailure(t *testing.T) {
	s, fault := openNotificationFault(t)
	ctx := context.Background()
	out := send(t, s, msg(bob, ana, "ack-after-notification-failure"), t0)
	fault.enable(t)
	_, err := s.Ack(ctx, bob, []int64{out.Seq}, t0)
	fault.recovered(t, err)
	if ds := receive(t, s, bob, t0); len(ds) != 1 || ds[0].Seq != out.Seq {
		t.Fatalf("failed ack removed the delivery: %+v", ds)
	}
	if n, err := s.Ack(ctx, bob, []int64{out.Seq}, t0); err != nil || n != 1 {
		t.Fatalf("retried ack: %d, %v", n, err)
	}
	if ds := receive(t, s, bob, t0); len(ds) != 0 {
		t.Fatalf("retried ack retained a delivery: %+v", ds)
	}
}

func TestMailboxExpireNotificationFailure(t *testing.T) {
	s, fault := openNotificationFault(t)
	ctx := context.Background()
	m := msg(bob, ana, "expiry-after-notification-failure")
	m.Deadline = t0.Add(time.Hour)
	out := send(t, s, m, t0)
	later := t0.Add(2 * time.Hour)
	fault.enable(t)
	_, err := s.Expire(ctx, later)
	fault.recovered(t, err)
	if ds := receive(t, s, bob, t0); len(ds) != 1 || ds[0].Seq != out.Seq {
		t.Fatalf("failed expiry removed the original row: %+v", ds)
	}
	if ds := receive(t, s, ana, later); len(ds) != 0 {
		t.Fatalf("failed expiry retained a notice: %+v", ds)
	}
	exp, err := s.Expire(ctx, later)
	if err != nil || len(exp) != 1 || exp[0].Seq != out.Seq {
		t.Fatalf("retried expiry: %+v, %v", exp, err)
	}
	if exp, err := s.Expire(ctx, later); err != nil || len(exp) != 0 {
		t.Fatalf("repeated expiry: %+v, %v", exp, err)
	}
	if ds := receive(t, s, bob, later); len(ds) != 0 {
		t.Fatalf("retried expiry retained the delivery: %+v", ds)
	}
	if ds := receive(t, s, ana, later); len(ds) != 1 || ds[0].Kind != mailbox.KindExpired {
		t.Fatalf("retried expiry must leave exactly one notice: %+v", ds)
	}
}
