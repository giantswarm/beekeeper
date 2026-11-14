package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/giantswarm/beekeeper/internal/migrations"
	"github.com/giantswarm/beekeeper/internal/testing/pgtest"
)

// The database roles the migrations grant to.
const (
	roleEngine   = "engine"
	roleReceiver = "receiver"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestAllNumberedInOrder(t *testing.T) {
	t.Parallel()
	all, err := migrations.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 || all[0].Version != 1 {
		t.Fatalf("migrations = %+v, want version 1 first", all)
	}
	for i := 1; i < len(all); i++ {
		if all[i].Version <= all[i-1].Version {
			t.Fatalf("versions out of order: %d after %d", all[i].Version, all[i-1].Version)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	t.Parallel()
	db := pgtest.NewDB(t)
	ctx := context.Background()
	// The template was migrated once already; again applies nothing and still succeeds (grants rerun).
	applied, err := migrations.Apply(ctx, db.Pool())
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Fatalf("applied %v on a migrated database", applied)
	}
	all, _ := migrations.All()
	var n int
	if err := db.Pool().QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(all) {
		t.Fatalf("schema_migrations has %d rows, want %d", n, len(all))
	}
}

func TestTablesAndColumns(t *testing.T) {
	t.Parallel()
	db := pgtest.NewDB(t)
	ctx := context.Background()
	want := map[string][]string{
		"inbox":     {"delivery_id", "event", "action", "received_at", "payload", "processed_at"},
		"events":    {"id", "at", "team", "item", "change", "type", "source", "data"},
		"decisions": {"id", "at", "point", "subject", "other", "inputs", "inputs_hash", "rule", "jev", "jev_confidence", "jev_error", "chosen", "mode", "outcome", "team"},
		"durations": {"kind", "key", "value", "at"},
	}
	for table, cols := range want {
		rows, err := db.Pool().Query(ctx, "SELECT column_name FROM information_schema.columns WHERE table_name = $1", table)
		if err != nil {
			t.Fatal(err)
		}
		have, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		set := map[string]bool{}
		for _, c := range have {
			set[c] = true
		}
		for _, c := range cols {
			if !set[c] {
				t.Errorf("%s has no column %s (has %v)", table, c, have)
			}
		}
	}
	// Indexes the plan names.
	for _, idx := range []string{"events_item_at", "events_change_at", "events_type_at", "decisions_key"} {
		var n int
		if err := db.Pool().QueryRow(ctx, "SELECT count(*) FROM pg_indexes WHERE indexname = $1", idx).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("index %s missing", idx)
		}
	}
}

// The receiver role may only INSERT into inbox (with ON CONFLICT DO NOTHING); the engine role may do
// everything.
func TestRoleGrants(t *testing.T) {
	t.Parallel()
	db := pgtest.NewDB(t)
	ctx := context.Background()
	as := func(role, sql string, args ...any) error {
		tx, err := db.Pool().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }() // after Commit it only reports the transaction closed
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, sql, args...)
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	insert := "INSERT INTO inbox (delivery_id, event, action, received_at, payload) VALUES ($1, 'issues', 'edited', $2, '{}') ON CONFLICT DO NOTHING"
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		role, sql string
		args      []any
		denied    bool
	}{
		{roleReceiver, insert, []any{"r-1", now}, false},
		{roleReceiver, insert, []any{"r-1", now}, false}, // duplicate: still allowed, inserts nothing
		{roleReceiver, "SELECT * FROM inbox", nil, true},
		{roleReceiver, "UPDATE inbox SET processed_at = now()", nil, true},
		{roleReceiver, "DELETE FROM inbox", nil, true},
		{roleReceiver, "INSERT INTO events (at, type) VALUES (now(), 'x')", nil, true},
		{roleReceiver, "SELECT * FROM decisions", nil, true},
		{roleEngine, insert, []any{"e-1", now}, false},
		{roleEngine, "UPDATE inbox SET processed_at = $1", []any{now}, false},
		{roleEngine, "DELETE FROM inbox WHERE delivery_id = 'e-1'", nil, false},
		{roleEngine, "INSERT INTO events (at, type) VALUES ($1, 'x')", []any{now}, false},
		{roleEngine, "INSERT INTO decisions (at, point, subject, inputs_hash) VALUES ($1, 'p', 's', 'h')", []any{now}, false},
		{roleEngine, "INSERT INTO durations (kind, key, value, at) VALUES ('ci', 'r', '20 minutes', $1)", []any{now}, false},
		{roleEngine, "SELECT * FROM events, decisions, durations, inbox", nil, false},
	}
	for _, c := range cases {
		err := as(c.role, c.sql, c.args...)
		denied := err != nil && strings.Contains(err.Error(), "permission denied")
		if err != nil && !denied {
			t.Errorf("%s: %s: %v", c.role, c.sql, err)
			continue
		}
		if denied != c.denied {
			t.Errorf("%s: %s: denied = %v, want %v", c.role, c.sql, denied, c.denied)
		}
	}
}

func TestInboxInsertNotifies(t *testing.T) {
	t.Parallel()
	db := pgtest.NewDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, db.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }() // the test is over: nothing to report
	if _, err := conn.Exec(ctx, "LISTEN inbox"); err != nil {
		t.Fatal(err)
	}
	ins := "INSERT INTO inbox (delivery_id, event, received_at, payload) VALUES ($1, 'push', now(), '{}') ON CONFLICT DO NOTHING"
	for _, id := range []string{"d-1", "d-1", "d-2"} {
		if _, err := db.Pool().Exec(ctx, ins, id); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for range 2 {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, n.Payload)
	}
	if strings.Join(got, ",") != "d-1,d-2" {
		t.Fatalf("notifications = %v, want d-1,d-2 (none for the duplicate)", got)
	}
}

// Inbox payloads are bytes (0002): a delivery stored as jsonb before keeps its JSON, and a NUL escape,
// which jsonb refuses, is stored as sent.
func TestInboxPayloadIsBytes(t *testing.T) {
	t.Parallel()
	db := pgtest.NewDB(t)
	ctx := context.Background()
	var typ string
	if err := db.Pool().QueryRow(ctx, "SELECT data_type FROM information_schema.columns WHERE table_name = 'inbox' AND column_name = 'payload'").Scan(&typ); err != nil {
		t.Fatal(err)
	}
	if typ != "bytea" {
		t.Fatalf("inbox.payload is %s, want bytea", typ)
	}
	all, err := migrations.All()
	if err != nil {
		t.Fatal(err)
	}
	var m0002 string
	for _, m := range all {
		if m.Version == 2 {
			m0002 = m.SQL
		}
	}
	if m0002 == "" {
		t.Fatal("no migration 0002")
	}
	nul := `{"body":"a\u0000b"}`
	// Back to 0001's jsonb with a stored delivery, then 0002 again, in a transaction that is rolled back.
	err = pgx.BeginFunc(ctx, db.Pool(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "ALTER TABLE inbox ALTER COLUMN payload TYPE jsonb USING convert_from(payload, 'UTF8')::jsonb"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO inbox (delivery_id, event, received_at, payload) VALUES ('old', 'issues', now(), '{"action": "edited"}')`); err != nil {
			return err
		}
		err := pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error {
			_, err := sp.Exec(ctx, "INSERT INTO inbox (delivery_id, event, received_at, payload) VALUES ('nul', 'issues', now(), $1)", nul)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "22P05") {
			return fmt.Errorf("jsonb and a NUL escape: %v, want SQLSTATE 22P05", err)
		}
		if _, err := tx.Exec(ctx, m0002); err != nil {
			return fmt.Errorf("0002: %w", err)
		}
		var old []byte
		if err := tx.QueryRow(ctx, "SELECT payload FROM inbox WHERE delivery_id = 'old'").Scan(&old); err != nil {
			return err
		}
		if string(old) != `{"action": "edited"}` {
			return fmt.Errorf("converted payload %q", old)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO inbox (delivery_id, event, received_at, payload) VALUES ('nul', 'issues', now(), $1)", []byte(nul)); err != nil {
			return fmt.Errorf("bytea and a NUL escape: %w", err)
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
}

var errRollback = errors.New("rollback")
