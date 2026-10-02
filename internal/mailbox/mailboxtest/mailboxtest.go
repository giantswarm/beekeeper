// Package mailboxtest gives a test a database of its own on the Postgres
// BEEKEEPER_TEST_DATABASE_URL names (docs/development.md starts one).
package mailboxtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Env is the variable naming the test Postgres.
const Env = "BEEKEEPER_TEST_DATABASE_URL"

// URL creates an empty database for the test, dropped at its end, and
// returns its URL. Without Env set the test is skipped.
func URL(t *testing.T) string {
	t.Helper()
	base := os.Getenv(Env)
	if base == "" {
		t.Skip(Env + " is not set: a Postgres for the mailbox tests, see docs/development.md")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "beekeeper_test_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}
