package mailbox_test

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/mailbox"
	"github.com/giantswarm/beekeeper/internal/mailbox/mailboxtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// notifyFault shadows pg_notify only in this test's database. Explicitly
// placing pg_catalog last makes the shadow visible to every Store connection.
// Forwarding before raising also exercises rollback of pending notifications.
type notifyFault struct {
	t            *testing.T
	ctx          context.Context
	db, listener *pgx.Conn
	store        *mailbox.Store
}

func newNotifyFault(t *testing.T) *notifyFault {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	u, err := url.Parse(mailboxtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", "public,pg_catalog")
	u.RawQuery = q.Encode()
	f := &notifyFault{t: t, ctx: ctx}
	f.store = open(t, u.String())
	f.db, err = pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.db.Close(context.Background()) })
	f.exec(`CREATE TABLE notify_fault (payload text);
 CREATE FUNCTION public.pg_notify(channel text, payload text) RETURNS void LANGUAGE plpgsql AS $$
 BEGIN
   PERFORM pg_catalog.pg_notify(channel, payload);
   IF EXISTS (SELECT 1 FROM notify_fault WHERE notify_fault.payload = pg_notify.payload) THEN
     RAISE EXCEPTION 'injected notification failure' USING ERRCODE = 'P0001';
   END IF;
 END $$`)
	f.listener, err = pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.listener.Close(context.Background()) })
	_, err = f.listener.Exec(ctx, "LISTEN "+mailbox.Channel)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *notifyFault) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *notifyFault) failed(err error) {
	f.t.Helper()
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "P0001" || pgerr.Message != "injected notification failure" {
		f.t.Fatalf("got %v, want injected notification failure", err)
	}
	f.notifications()
}

// The committed marker is ordered after all preceding transactions' notices.
// Reaching it bounds the absence assertion without a sleep or polling.
func (f *notifyFault) notifications(want ...string) {
	f.t.Helper()
	const marker = "rollback-test-marker"
	f.exec(`SELECT pg_catalog.pg_notify($1, $2)`, mailbox.Channel, marker)
	var got []string
	for {
		n, err := f.listener.WaitForNotification(f.ctx)
		if err != nil {
			f.t.Fatal(err)
		}
		if n.Payload == marker {
			break
		}
		got = append(got, n.Payload)
	}
	if !reflect.DeepEqual(got, want) {
		f.t.Fatalf("notifications %v, want %v", got, want)
	}
}

func (f *notifyFault) state(messages, dedupe int) {
	f.t.Helper()
	var m, d int
	if err := f.db.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM messages), (SELECT count(*) FROM dedupe)`).Scan(&m, &d); err != nil {
		f.t.Fatal(err)
	}
	if m != messages || d != dedupe {
		f.t.Fatalf("rows messages=%d dedupe=%d, want %d %d", m, d, messages, dedupe)
	}
}

func TestMailboxSendNotificationRollback(t *testing.T) {
	f := newNotifyFault(t)
	f.exec(`INSERT INTO notify_fault VALUES ($1)`, bobKey)
	_, err := f.store.Send(f.ctx, msg(bob, ana, "rollback-send"), t0)
	f.failed(err)
	f.state(0, 0)
	if ds := receive(t, f.store, bob, t0); len(ds) != 0 {
		t.Fatalf("failed send queued %+v", ds)
	}
	f.exec(`DELETE FROM notify_fault`)
	sent := send(t, f.store, msg(bob, ana, "rollback-send"), t0)
	if sent.Duplicate {
		t.Fatal("failed send remembered the ID")
	}
	f.state(1, 1)
	if ds := receive(t, f.store, bob, t0); len(ds) != 1 || ds[0].Seq != sent.Seq {
		t.Fatalf("retry delivery %+v", ds)
	}
	f.notifications(bobKey)
	if again := send(t, f.store, msg(bob, ana, "rollback-send"), t0); !again.Duplicate || again.Seq != sent.Seq {
		t.Fatalf("retry dedupe %+v", again)
	}
	f.notifications()
}

func TestMailboxAckNotificationRollback(t *testing.T) {
	f := newNotifyFault(t)
	sent := send(t, f.store, msg(bob, ana, "rollback-ack"), t0)
	f.notifications(bobKey)
	before := receive(t, f.store, bob, t0)
	f.exec(`INSERT INTO notify_fault VALUES ($1)`, bobKey)
	_, err := f.store.Ack(f.ctx, bob, []int64{sent.Seq}, t0)
	f.failed(err)
	f.state(1, 1)
	if ds := receive(t, f.store, bob, t0); !reflect.DeepEqual(ds, before) {
		t.Fatalf("failed ack changed delivery: %+v", ds)
	}
	f.exec(`DELETE FROM notify_fault`)
	if n, err := f.store.Ack(f.ctx, bob, []int64{sent.Seq}, t0); err != nil || n != 1 {
		t.Fatalf("retry ack: %d, %v", n, err)
	}
	if ds := receive(t, f.store, bob, t0); len(ds) != 0 {
		t.Fatalf("acked delivery %+v", ds)
	}
	if again := send(t, f.store, msg(bob, ana, "rollback-ack"), t0); !again.Duplicate {
		t.Fatal("ack lost dedupe")
	}
	f.notifications(bobKey)
}

func TestMailboxExpireNotificationRollback(t *testing.T) {
	f := newNotifyFault(t)
	m := msg(bob, ana, "rollback-expire")
	m.Deadline = t0.Add(time.Hour)
	sent := send(t, f.store, m, t0)
	f.notifications(bobKey)
	before := receive(t, f.store, bob, t0)
	// Fail the final recipient change, after the sender notice was inserted
	// and its notification was queued successfully in the same transaction.
	f.exec(`INSERT INTO notify_fault VALUES ($1)`, bobKey)
	later := t0.Add(2 * time.Hour)
	_, err := f.store.Expire(f.ctx, later)
	f.failed(err)
	f.state(1, 1)
	if ds := receive(t, f.store, bob, t0); !reflect.DeepEqual(ds, before) {
		t.Fatalf("failed expiry changed original: %+v", ds)
	}
	if ds := receive(t, f.store, ana, later); len(ds) != 0 {
		t.Fatalf("failed expiry left notices %+v", ds)
	}
	if again := send(t, f.store, m, t0); !again.Duplicate || again.Seq != sent.Seq {
		t.Fatalf("failed expiry lost dedupe: %+v", again)
	}
	f.exec(`DELETE FROM notify_fault`)
	expired, err := f.store.Expire(f.ctx, later)
	if err != nil || len(expired) != 1 || expired[0].Seq != sent.Seq {
		t.Fatalf("retry expiry: %+v, %v", expired, err)
	}
	f.notifications(ana, bobKey)
	f.state(1, 1)
	notices := receive(t, f.store, ana, later)
	if len(notices) != 1 || notices[0].Kind != mailbox.KindExpired {
		t.Fatalf("retry notices %+v", notices)
	}
	if ds := receive(t, f.store, bob, t0); len(ds) != 0 {
		t.Fatalf("expired original retained %+v", ds)
	}
	if expired, err := f.store.Expire(f.ctx, later); err != nil || len(expired) != 0 {
		t.Fatalf("further sweep: %+v, %v", expired, err)
	}
	if ds := receive(t, f.store, ana, later); !reflect.DeepEqual(ds, notices) {
		t.Fatalf("further sweep changed notices: %+v", ds)
	}
	f.notifications()
	f.state(1, 1)
}
