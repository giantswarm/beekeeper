package mailbox

import (
	"context"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/mailbox/mailboxtest"
	"github.com/jackc/pgx/v5"
)

func TestMailboxUpgradeRetainsReusedID(t *testing.T) {
	ctx := context.Background()
	url := mailboxtest.URL(t)
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	// Start at the original schema, including its lifetime uniqueness constraint.
	if _, err := conn.Exec(ctx, migrations[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE schema_version (version int NOT NULL); INSERT INTO schema_version VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	m := Message{Mailbox: "recipient@example.com", Sender: "sender@example.com", ID: "reused", Envelope: []byte(`{"text":"original"}`), Deadline: now.Add(72 * time.Hour)}
	var first int64
	if err := conn.QueryRow(ctx, `INSERT INTO messages (mailbox, sender, sender_team, kind, message_id, envelope, sent, deadline)
		VALUES ($1, $2, '', $3, $4, $5, $6, $7) RETURNING seq`, m.Mailbox, m.Sender, KindMessage, m.ID, []byte(m.Envelope), now, m.Deadline).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO dedupe VALUES ($1, $2, $3, $4)`, m.Sender, m.ID, first, now); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if duplicate, err := s.Send(ctx, m, now.Add(time.Hour)); err != nil || !duplicate.Duplicate || duplicate.Seq != first {
		t.Fatalf("upgraded dedupe: %+v, %v", duplicate, err)
	}
	later := now.Add(DedupeWindow + time.Hour)
	second, err := s.Send(ctx, m, later)
	if err != nil || second.Duplicate || second.Seq <= first {
		t.Fatalf("new delivery: %+v, %v", second, err)
	}
	ds, err := s.Receive(ctx, m.Mailbox, 0, later)
	if err != nil || len(ds) != 2 || ds[0].Seq != first || ds[1].Seq != second.Seq || string(ds[0].Envelope) != `{"text": "original"}` {
		t.Fatalf("upgraded deliveries: %+v, %v", ds, err)
	}
	// Opening an already upgraded database must also succeed.
	reopened, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
}
