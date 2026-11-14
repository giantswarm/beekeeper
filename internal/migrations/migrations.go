// Package migrations holds the engine's Postgres schema (plan §5) as embedded SQL and applies it.
// `beekeeper migrate` runs it as the engine's init container; the test harness migrates a template
// database once per test binary.
package migrations

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed sql/*.sql
var files embed.FS

// lockID serializes concurrent migrators (two engine replicas starting at once).
const lockID = 0x6265656b // "beek"

// Migration is one numbered schema change.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// All returns the numbered migrations in order (not the grants, which run after every Migrate).
func All() ([]Migration, error) {
	entries, err := fs.Glob(files, "sql/*.sql")
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, path := range entries {
		base := strings.TrimSuffix(strings.TrimPrefix(path, "sql/"), ".sql")
		num, name, ok := strings.Cut(base, "_")
		if !ok {
			continue // grants.sql
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("migration %s: version %q is not a number", path, num)
		}
		b, err := files.ReadFile(path)
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Version: v, Name: name, SQL: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i := 1; i < len(out); i++ {
		if out[i].Version == out[i-1].Version {
			return nil, fmt.Errorf("migration version %d appears twice", out[i].Version)
		}
	}
	return out, nil
}

// Grants returns the privileges statement that runs after every Migrate: GRANTs for the roles `engine`
// (everything) and `receiver` (INSERT on inbox), each only when the role exists.
func Grants() string {
	b, err := files.ReadFile("sql/grants.sql")
	if err != nil {
		panic(err) // embedded
	}
	return string(b)
}

// Migrate applies every migration not yet applied, each in its own transaction, then the grants. It holds
// an advisory lock while it runs, so concurrent migrators apply each migration once.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := Apply(ctx, pool)
	return err
}

// Apply is Migrate, reporting the versions it applied.
func Apply(ctx context.Context, pool *pgxpool.Pool) (applied []int, err error) {
	all, err := All()
	if err != nil {
		return nil, err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockID); err != nil {
		return nil, fmt.Errorf("migrate: lock: %w", err)
	}
	defer func() {
		if _, uerr := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lockID); uerr != nil && err == nil {
			err = fmt.Errorf("migrate: unlock: %w", uerr)
		}
	}()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    integer PRIMARY KEY,
		name       text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return nil, fmt.Errorf("migrate: bookkeeping table: %w", err)
	}
	done := map[int]bool{}
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	for _, v := range versions {
		done[int(v)] = true
	}
	for _, m := range all {
		if done[m.Version] {
			continue
		}
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, m.SQL); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", m.Version, m.Name)
			return err
		})
		if err != nil {
			return applied, fmt.Errorf("migrate: %04d_%s: %w", m.Version, m.Name, err)
		}
		applied = append(applied, m.Version)
	}
	if _, err := conn.Exec(ctx, Grants()); err != nil {
		return applied, fmt.Errorf("migrate: grants: %w", err)
	}
	return applied, nil
}
