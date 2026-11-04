package mailbox_test

import (
	"context"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/mailbox"
	"github.com/giantswarm/beekeeper/internal/mailbox/mailboxtest"
	"github.com/jackc/pgx/v5"
)

// awaitListener bounds every lifecycle transition, including cleanup after a
// failed assertion. Buffered callback channels keep delivery independent of assertions.
func awaitListener[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("listener lifecycle transition timed out")
		var zero T
		return zero
	}
}

func TestMailboxListenLifecycle(t *testing.T) {
	for _, cancelOnLoss := range []bool{false, true} {
		name := "reconnect"
		if cancelOnLoss {
			name = "cancel_on_loss"
		}
		t.Run(name, func(t *testing.T) {
			url := mailboxtest.URL(t)
			s := open(t, url)
			// Control queries must survive cancellation from the loss callback.
			controlCtx, controlCancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer controlCancel()
			control, err := pgx.Connect(controlCtx, url)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = control.Close(context.Background()) }()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			ready := make(chan struct{}, 4)
			lost := make(chan error, 4)
			changed := make(chan string, 4)
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.Listen(ctx,
					func(mb string) {
						select {
						case changed <- mb:
						case <-ctx.Done():
						}
					},
					func() {
						select {
						case ready <- struct{}{}:
						default:
						}
					},
					func(err error) {
						if cancelOnLoss {
							cancel()
						}
						select {
						case lost <- err:
						case <-controlCtx.Done():
						}
					})
			}()
			t.Cleanup(func() { cancel(); awaitListener(t, done) })
			awaitListener(t, ready)

			// Only the dedicated connection in this newly created database has
			// executed LISTEN. Require exactly one match before terminating it.
			rows, err := control.Query(controlCtx, `SELECT pid FROM pg_stat_activity
    WHERE datname = current_database() AND pid <> pg_backend_pid()
    AND backend_type = 'client backend' AND query = $1`, "LISTEN "+mailbox.Channel)
			if err != nil {
				t.Fatal(err)
			}
			pids, err := pgx.CollectRows(rows, pgx.RowTo[int32])
			if err != nil {
				t.Fatal(err)
			}
			if len(pids) != 1 {
				t.Fatalf("found %d fixture listeners, want 1", len(pids))
			}
			var terminated bool
			if err := control.QueryRow(controlCtx, `SELECT pg_terminate_backend(pid)
    FROM pg_stat_activity WHERE pid = $1 AND datname = current_database()
    AND query = $2`, pids[0], "LISTEN "+mailbox.Channel).Scan(&terminated); err != nil {
				t.Fatal(err)
			}
			if !terminated {
				t.Fatal("fixture listener was not terminated")
			}
			if err := awaitListener(t, lost); err == nil {
				t.Fatal("lost callback received nil error")
			}

			if !cancelOnLoss {
				awaitListener(t, ready)
				if _, err := s.Send(controlCtx, msg(bob, ana, "after-reconnect"), t0); err != nil {
					t.Fatal(err)
				}
				if mb := awaitListener(t, changed); mb != bobKey {
					t.Fatalf("replacement listener announced %q", mb)
				}
				cancel()
			}
			awaitListener(t, done)
			select {
			case <-ready:
				t.Fatal("unexpected extra registration")
			default:
			}
			select {
			case err := <-lost:
				t.Fatalf("unexpected extra loss: %v", err)
			default:
			}
			// Listen owns a separate connection; stopping it must leave Send usable.
			if _, err := s.Send(controlCtx, msg(bob, ana, "after-stop"), t0); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMailboxListenAlreadyCanceled(t *testing.T) {
	s := open(t, mailboxtest.URL(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	callbacks := make(chan struct{}, 3)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Listen(ctx, func(string) { callbacks <- struct{}{} }, func() { callbacks <- struct{}{} }, func(error) { callbacks <- struct{}{} })
	}()
	awaitListener(t, done)
	select {
	case <-callbacks:
		t.Fatal("already-canceled listener called a callback")
	default:
	}
	sendCtx, sendCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer sendCancel()
	if _, err := s.Send(sendCtx, msg(bob, ana, "after-canceled-listen"), t0); err != nil {
		t.Fatal(err)
	}
}
