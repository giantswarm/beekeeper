package pgtest_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/testing/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestFreshDatabasesAreIsolated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, b := pgtest.NewDB(t), pgtest.NewDB(t)
	if a.Name() == b.Name() {
		t.Fatal("two tests share a database")
	}
	if _, err := a.Pool().Exec(ctx, "INSERT INTO events (at, type) VALUES ($1, 'x')", time.Now()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.Pool().QueryRow(ctx, "SELECT count(*) FROM events").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("b sees %d of a's rows", n)
	}
}

func TestDownAndUp(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db := pgtest.NewDB(t)
	ping := func() error {
		c, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_, err := db.Pool().Exec(c, "SELECT 1")
		return err
	}
	if err := ping(); err != nil {
		t.Fatal(err)
	}
	if err := db.Down(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := ping(); err == nil {
		t.Fatal("query succeeded while Postgres is down")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("failing took %s; down must fail fast", d)
	}
	if err := db.Up(); err != nil {
		t.Fatal(err)
	}
	if err := ping(); err != nil {
		t.Fatalf("after Up: %v", err)
	}
}

// Many tests at once each get a database quickly.
func TestManyParallelDatabases(t *testing.T) {
	t.Parallel()
	for i := range 40 {
		t.Run("", func(t *testing.T) {
			t.Parallel()
			start := time.Now()
			db := pgtest.NewDB(t)
			if _, err := db.Pool().Exec(context.Background(), "INSERT INTO inbox (delivery_id, event, received_at, payload) VALUES ($1, 'push', now(), '{}')", fmt.Sprint(i)); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Errorf("database took %s", d)
			}
		})
	}
}
