package mailbox

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/giantswarm/beekeeper/internal/mailbox/mailboxtest"
)

func TestMailboxUpgradeKeepsPendingDelivery(t *testing.T) {
	ctx := context.Background()
	url := mailboxtest.URL(t)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// Construct the original schema, including its lifetime uniqueness
	// constraint, and queue a long-lived delivery before upgrading.
	if _, err := pool.Exec(ctx, migrations[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE schema_version (version int NOT NULL); INSERT INTO schema_version VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	m := Message{Mailbox: "upgrade-recipient@example.com", Sender: "upgrade-sender@example.com", ID: "reused-after-upgrade",
		Envelope: []byte(`{"text":"kept through upgrade"}`), Deadline: now.Add(3 * DedupeWindow)}
	old := &Store{pool: pool}
	first, err := old.Send(ctx, m, now)
	if err != nil {
		t.Fatal(err)
	}
	old.Close()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	later := now.Add(DedupeWindow + time.Hour)
	second, err := s.Send(ctx, m, later)
	if err != nil || second.Duplicate {
		t.Fatalf("reuse after upgrading: %+v, %v", second, err)
	}
	ds, err := s.Receive(ctx, m.Mailbox, 0, later)
	if err != nil || len(ds) != 2 || ds[0].Seq != first.Seq || ds[1].Seq != second.Seq {
		t.Fatalf("upgrade lost a pending delivery: %+v, %v", ds, err)
	}
}
