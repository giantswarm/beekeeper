// Package cmd holds the flags and wiring of each role of the beekeeper binary; the logic lives in
// internal/. Roles: engine, receiver, migrate, replay, version.
package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/go-logr/logr"
	"github.com/jackc/pgx/v5/pgxpool"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/giantswarm/beekeeper/pkg/project"
)

// command is one subcommand.
type command struct {
	summary string
	run     func(ctx context.Context, args []string, stdout, stderr io.Writer) error
}

var commands = map[string]command{
	"engine":   {"run one engine replica (leader-elected)", runEngine},
	"receiver": {"serve the GitHub webhook route and store deliveries in the inbox", runReceiver},
	"migrate":  {"apply the Postgres migrations (safe to run concurrently)", runMigrate},
	"replay":   {"re-run every recorded decision from its recorded inputs", runReplay},
	"version":  {"print the version", runVersion},
}

// Execute runs the subcommand named by args[0] and returns the exit code.
func Execute(args []string) int { return execute(args, os.Stdout, os.Stderr) }

func execute(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(stderr)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	c, ok := commands[args[0]]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n\n", args[0]) // the exit code reports the failure
		usage(stderr)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := c.run(ctx, args[1:], stdout, stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		_, _ = fmt.Fprintf(stderr, "beekeeper %s: %v\n", args[0], err) // the exit code reports the failure
		return 1
	}
	return 0
}

// usage writes the help text; a failed write to the terminal has nowhere better to be reported.
func usage(w io.Writer) {
	_, _ = fmt.Fprintf(w, "%s %s: %s\n\nUsage: beekeeper <command> [flags]\n\nCommands:\n", project.Name(), project.Version(),
		project.Description())
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		_, _ = fmt.Fprintf(w, "  %-9s %s\n", n, commands[n].summary)
	}
}

// flagSet returns a FlagSet for a subcommand that reports errors instead of exiting.
func flagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("beekeeper "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// env returns the value of the first set environment variable, else def.
func env(def string, names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return def
}

// newLogger returns a JSON logger at level (debug, info, warn, error) on stderr, set as
// controller-runtime's.
func newLogger(level string) (logr.Logger, error) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "", "info":
		lvl = slog.LevelInfo
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return logr.Discard(), fmt.Errorf("unknown log level %q (debug, info, warn, error)", level)
	}
	l := logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	ctrl.SetLogger(l)
	return l, nil
}

// newPool connects to Postgres. An empty DSN takes everything from the standard libpq environment
// (PGHOST, PGPORT, PGUSER, PGPASSWORD, PGDATABASE, PGSSLMODE).
func newPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	return pool, nil
}

// readFile reads a mounted secret, trimming a trailing newline.
func readFile(path, what string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("no %s file given", what)
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: the operator names the mounted secret's path in a flag
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	return []byte(strings.TrimRight(string(b), "\r\n")), nil
}

func runVersion(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flagSet("version", stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, err := fmt.Fprintf(stdout, "%s %s (%s) %s\n", project.Name(), project.Version(), project.GitSHA(), project.Source())
	return err
}
