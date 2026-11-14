// Package pgtest gives tests a real Postgres (contract §3.1): one embedded server per test binary, from
// the cached embedded-postgres binaries (never downloaded), a template database migrated once, and a
// fresh database per test created from the template in milliseconds. Each database is reached through
// an in-process TCP proxy, so a test can take Postgres down and bring it back (postgres_fault).
//
// Use it from TestMain:
//
//	func TestMain(m *testing.M) { pgtest.Main(m) }
package pgtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/giantswarm/beekeeper/internal/migrations"
)

const (
	version  = embeddedpostgres.V18
	user     = "postgres"
	password = "postgres"
	// Template is the migrated database every test database is copied from.
	Template = "beekeeper_template"
	// MaxConns bounds each test database's Pool, so ~120 parallel scenarios stay well under the server's
	// max_connections.
	MaxConns = 8
)

// Roles the CloudNativePG operator creates in production; the server has them so the grants are
// migrated (and testable with SET ROLE).
var Roles = []string{"engine", "receiver"}

type server struct {
	ep      *embeddedpostgres.EmbeddedPostgres
	port    uint32
	dir     string
	bins    string
	logFile string
	admin   *pgxpool.Pool
	created atomic.Int64
}

var (
	mu  sync.Mutex
	srv *server
)

// Main starts the server, runs the tests and stops the server: TestMain's whole body.
func Main(m *testing.M) {
	stop, err := Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "pgtest:", err)
		os.Exit(1)
	}
	code := m.Run()
	stop()
	os.Exit(code)
}

// Start starts this test binary's Postgres server on a free port, with its data directory under
// $PGTEST_DIR (default <temp dir>/beekeeper-pgtest; each live database takes ~8 MB there), and migrates
// the template database. Call it once from TestMain and call stop after m.Run. PGTEST_KEEP_LOG=1 keeps
// the server's log and prints where.
func Start() (stop func(), err error) {
	mu.Lock()
	defer mu.Unlock()
	if srv != nil {
		return nil, errors.New("pgtest: Start called twice")
	}
	s, err := start()
	if err != nil {
		return nil, err
	}
	srv = s
	return func() {
		mu.Lock()
		defer mu.Unlock()
		if srv == nil {
			return
		}
		srv.stop()
		srv = nil
	}, nil
}

// DepsDir is where the test binaries are cached: $BEEKEEPER_TEST_DEPS, else
// <user cache dir>/beekeeper/test-deps (filled by `make test-deps`).
func DepsDir() string {
	if d := os.Getenv("BEEKEEPER_TEST_DEPS"); d != "" {
		return d
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = filepath.Join(os.TempDir(), "cache")
	}
	return filepath.Join(base, "beekeeper", "test-deps")
}

func start() (*server, error) {
	cache := filepath.Join(DepsDir(), "postgres")
	archives, _ := filepath.Glob(filepath.Join(cache, "embedded-postgres-binaries-*-"+string(version)+".txz"))
	bins := filepath.Join(cache, string(version))
	extracted := fileExists(filepath.Join(bins, ".complete")) && fileExists(filepath.Join(bins, "bin", "pg_ctl"))
	if !extracted && len(archives) == 0 {
		return nil, fmt.Errorf("no cached Postgres %s binaries in %s: run `make test-deps` (tests never download)", version, cache)
	}

	root := os.Getenv("PGTEST_DIR")
	if root == "" {
		root = filepath.Join(os.TempDir(), "beekeeper-pgtest")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, err
	}
	reapStale(root, bins)
	dir, err := os.MkdirTemp(root, strconv.Itoa(os.Getpid())+"-")
	if err != nil {
		return nil, err
	}
	// Without an extracted cache, embedded-postgres extracts the archive into the runtime directory.
	binaries := filepath.Join(dir, "runtime")
	if extracted {
		binaries = bins
	}

	const off = "off" // a Postgres boolean setting
	var lastErr error
	for range 5 {
		port, err := freePort()
		if err != nil {
			return nil, err
		}
		cfg := embeddedpostgres.DefaultConfig().
			Version(version).
			Port(port).
			Username(user).
			Password(password).
			Database("postgres").
			CachePath(cache).
			RuntimePath(filepath.Join(dir, "runtime")).
			BinariesPath(binaries).
			DataPath(filepath.Join(dir, "data")).
			Locale("C").
			Encoding("UTF8").
			StartTimeout(30 * time.Second).
			Logger(io.Discard).
			StartParameters(map[string]string{
				// Test data is disposable: trade durability for speed, and keep the WAL small.
				"fsync":                               off,
				"wal_level":                           "minimal",
				"max_wal_senders":                     "0",
				"max_wal_size":                        "64MB",
				"min_wal_size":                        "32MB",
				"synchronous_commit":                  off,
				"full_page_writes":                    off,
				"max_connections":                     "1000",
				"shared_buffers":                      "128MB",
				"max_locks_per_transaction":           "256",
				"unix_socket_directories":             "",
				"listen_addresses":                    "127.0.0.1",
				"log_min_messages":                    "warning",
				"idle_in_transaction_session_timeout": "0",
			})
		ep := embeddedpostgres.NewDatabase(cfg)
		if err := ep.Start(); err != nil {
			lastErr = err
			if strings.Contains(err.Error(), "already listening") {
				continue
			}
			break
		}
		s := &server{ep: ep, port: port, dir: dir, bins: binaries, logFile: serverLog(filepath.Join(dir, "data"))}
		if err := s.prepare(); err != nil {
			s.stop()
			return nil, err
		}
		return s, nil
	}
	_ = os.RemoveAll(dir) // best effort: the start error is the one to report, and a later run's reapStale removes the rest
	return nil, fmt.Errorf("pgtest: start Postgres: %w", lastErr)
}

func (s *server) dsn(db string) string {
	return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable", user, password, s.port, db)
}

// prepare creates the roles and the migrated template database.
func (s *server) prepare() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(s.dsn("postgres"))
	if err != nil {
		return err
	}
	cfg.MaxConns = 16
	admin, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	s.admin = admin
	for _, r := range Roles {
		if _, err := admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{r}.Sanitize()+" NOLOGIN"); err != nil {
			return fmt.Errorf("pgtest: create role %s: %w", r, err)
		}
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{Template}.Sanitize()); err != nil {
		return fmt.Errorf("pgtest: create template: %w", err)
	}
	tpl, err := pgxpool.New(ctx, s.dsn(Template))
	if err != nil {
		return err
	}
	err = migrations.Migrate(ctx, tpl)
	tpl.Close()
	if err != nil {
		return fmt.Errorf("pgtest: migrate template: %w", err)
	}
	// No connections to the template from now on: CREATE DATABASE … TEMPLATE needs it unused.
	if _, err := admin.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{Template}.Sanitize()+" WITH IS_TEMPLATE true ALLOW_CONNECTIONS false"); err != nil {
		return fmt.Errorf("pgtest: mark template: %w", err)
	}
	return nil
}

func (s *server) stop() {
	if s.admin != nil {
		s.admin.Close()
	}
	if err := s.ep.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "pgtest: stop Postgres:", err)
	}
	_ = os.RemoveAll(s.dir) // best effort, like the stop above: a later run's reapStale removes the rest
	if s.logFile != "" {
		if os.Getenv("PGTEST_KEEP_LOG") != "" {
			fmt.Fprintln(os.Stderr, "pgtest: server log kept in", s.logFile)
		} else {
			_ = os.Remove(s.logFile) // best effort: a leftover log only takes space
		}
	}
}

// serverLog finds the temp file embedded-postgres sends the server's output to (it never removes it):
// the postmaster's stderr (Linux only; elsewhere the file stays).
func serverLog(data string) string {
	b, err := os.ReadFile(filepath.Join(data, "postmaster.pid")) //nolint:gosec // G304: the pid file of a data directory this package created
	if err != nil {
		return ""
	}
	pid, _, _ := strings.Cut(string(b), "\n")
	target, err := os.Readlink("/proc/" + strings.TrimSpace(pid) + "/fd/2")
	if err != nil || !strings.HasPrefix(filepath.Base(target), "embedded_postgres_log") {
		return ""
	}
	return target
}

// DB is one test's database.
type DB struct {
	name  string
	proxy *proxy
	pool  *pgxpool.Pool
	dsn   string
}

// NewDB creates a fresh database from the migrated template, reached through a fault proxy. It is
// dropped when the test ends (after the test's own cleanups, which were registered later).
func NewDB(t testing.TB) *DB {
	t.Helper()
	mu.Lock()
	s := srv
	mu.Unlock()
	if s == nil {
		t.Fatal("pgtest: no server; call pgtest.Start (or pgtest.Main) from TestMain")
	}
	name := fmt.Sprintf("t%d_%d", os.Getpid(), s.created.Add(1))
	// FILE_COPY copies the template's files (~8 MB) without writing them to the WAL: under many parallel
	// tests it is several times faster than WAL_LOG, and a full disk fails one test instead of the server.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE "+pgx.Identifier{Template}.Sanitize()+" STRATEGY FILE_COPY"); err != nil {
		t.Fatalf("pgtest: create database: %v", err)
	}
	p, err := newProxy(fmt.Sprintf("127.0.0.1:%d", s.port))
	if err != nil {
		t.Fatalf("pgtest: proxy: %v", err)
	}
	db := &DB{
		name:  name,
		proxy: p,
		dsn:   fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable&connect_timeout=5", user, password, p.addr(), name),
	}
	cfg, err := pgxpool.ParseConfig(db.dsn)
	if err != nil {
		t.Fatalf("pgtest: %v", err)
	}
	cfg.MaxConns = MaxConns
	db.pool, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgtest: pool: %v", err)
	}
	t.Cleanup(func() {
		db.pool.Close()
		p.close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := s.admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("pgtest: drop database %s: %v", name, err)
		}
	})
	return db
}

// Name is the database's name.
func (d *DB) Name() string { return d.name }

// DSN is the database's connection string, through the proxy.
func (d *DB) DSN() string { return d.dsn }

// Pool is a pool on the database (through the proxy), closed when the test ends.
func (d *DB) Pool() *pgxpool.Pool { return d.pool }

// Down makes Postgres unreachable: every open connection is cut and new ones are refused at once.
// Pool's idle connections are dropped; other pools on DSN find theirs broken on next use. It never
// fails (the error is for the harness's Database interface).
func (d *DB) Down() error {
	d.proxy.setDown(true)
	d.pool.Reset()
	return nil
}

// Up makes Postgres reachable again.
func (d *DB) Up() error {
	d.proxy.setDown(false)
	d.pool.Reset()
	return nil
}

// IsDown reports whether Down is in effect.
func (d *DB) IsDown() bool { return d.proxy.isDown() }

func freePort() (uint32, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	// Only the port number is wanted; a failed close of the probe listener changes nothing.
	defer func() { _ = ln.Close() }()
	return uint32(ln.Addr().(*net.TCPAddr).Port), nil //nolint:gosec // G115: a TCP port fits in 16 bits
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// reapStale stops servers left behind by test binaries that died without stopping theirs (a test
// timeout panics past TestMain), and removes their directories. Runtime directories are named
// <pid>-<random>.
func reapStale(root, bins string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		pidStr, _, ok := strings.Cut(e.Name(), "-")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil || pid == os.Getpid() || processAlive(pid) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		data := filepath.Join(dir, "data")
		if fileExists(filepath.Join(data, "postmaster.pid")) {
			pgctl := filepath.Join(bins, "bin", "pg_ctl")
			if !fileExists(pgctl) {
				pgctl = filepath.Join(dir, "runtime", "bin", "pg_ctl")
			}
			_ = exec.Command(pgctl, "stop", "-m", "immediate", "-D", data).Run() //nolint:gosec // G204: pg_ctl of an embedded-postgres install under this package's own directory
		}
		_ = os.RemoveAll(dir) // best effort: the next run's reap tries again
	}
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
