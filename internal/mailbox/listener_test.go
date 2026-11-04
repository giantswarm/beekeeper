package mailbox_test

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/giantswarm/beekeeper/internal/mailbox"
	"github.com/giantswarm/beekeeper/internal/mailbox/mailboxtest"
)

func TestMailboxListenerRecovery(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cancelOnLoss bool
	}{
		{name: "case 0: reconnect and deliver"},
		{name: "case 1: cancel recovery", cancelOnLoss: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(mailboxtest.URL(t))
			if err != nil {
				t.Fatal(err)
			}
			const appName = "mailbox-listener-lifecycle"
			q := u.Query()
			q.Set("application_name", appName)
			u.RawQuery = q.Encode()
			s := open(t, u.String())
			testCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(stop)
			ctx, cancel := context.WithCancel(testCtx)
			t.Cleanup(cancel)
			control, err := pgx.Connect(testCtx, u.String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = control.Close(context.Background()) })
			ready := make(chan struct{}, 2)
			lost := make(chan error, 2)
			changed := make(chan string, 2)
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.Listen(ctx, func(mb string) { changed <- mb }, func() { ready <- struct{}{} }, func(err error) {
					if tc.cancelOnLoss {
						cancel()
					}
					lost <- err
				})
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("listener did not stop after cancellation")
				}
			})
			awaitReady := func() {
				t.Helper()
				select {
				case <-ready:
				case <-ctx.Done():
					t.Fatal("listener did not register:", ctx.Err())
				case <-done:
					t.Fatal("listener stopped before registration")
				}
			}
			awaitReady()
			// Match only the dedicated listener in this test's newly created
			// database. Pooled and control connections have other last queries.
			var pid int32
			err = control.QueryRow(testCtx, `SELECT pid FROM pg_stat_activity
				WHERE datname = current_database() AND application_name = $1 AND query = $2`,
				appName, "LISTEN "+mailbox.Channel).Scan(&pid)
			if err != nil {
				t.Fatal(err)
			}
			var terminated bool
			// Listener cancellation must not cancel the control query that
			// deliberately causes the loss it is observing.
			err = control.QueryRow(testCtx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
				WHERE pid = $1 AND datname = current_database() AND application_name = $2 AND query = $3`,
				pid, appName, "LISTEN "+mailbox.Channel).Scan(&terminated)
			if err != nil || !terminated {
				t.Fatalf("terminating the fixture listener: %t, %v", terminated, err)
			}
			select {
			case err := <-lost:
				if err == nil {
					t.Fatal("lost callback received no error")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("connection loss was not reported")
			}
			if !tc.cancelOnLoss {
				awaitReady()
				send(t, s, msg(bob, ana, "after-listener-reconnect"), t0)
				select {
				case mb := <-changed:
					if mb != bobKey {
						t.Fatalf("replacement listener announced %q", mb)
					}
				case <-ctx.Done():
					t.Fatal("replacement listener did not announce the send:", ctx.Err())
				}
				cancel()
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("listener did not stop after cancellation")
			}
			if len(ready) != 0 || len(lost) != 0 {
				t.Fatalf("unexpected callbacks after cancellation: ready=%d, lost=%d", len(ready), len(lost))
			}
			// Listen owns its dedicated connection, not the store's pool.
			send(t, s, msg(bob, ana, "after-listener-cancel"), t0)
		})
	}
}

func TestMailboxListenerAlreadyCanceled(t *testing.T) {
	s := open(t, mailboxtest.URL(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	callbacks := make(chan struct{}, 3)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Listen(ctx, func(string) { callbacks <- struct{}{} }, func() { callbacks <- struct{}{} }, func(error) { callbacks <- struct{}{} })
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not return for an already-canceled context")
	}
	if len(callbacks) != 0 {
		t.Fatal("listener called back for an already-canceled context")
	}
}
