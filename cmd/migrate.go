package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/clock"
	historypg "github.com/giantswarm/beekeeper/internal/history/pg"
	"github.com/giantswarm/beekeeper/internal/migrations"
	"github.com/giantswarm/beekeeper/internal/replay"
)

func runMigrate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flagSet("migrate", stderr)
	dsn := fs.String("postgres-dsn", env("", "BEEKEEPER_POSTGRES_DSN"), "Postgres DSN; empty takes the libpq environment (PGHOST, …)")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := newLogger(*logLevel); err != nil {
		return err
	}
	pool, err := newPool(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	// Migrate takes an advisory lock, so both engine replicas' init containers can run it at once.
	if err := migrations.Migrate(ctx, pool); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, "migrations applied") // they are applied: a lost line doesn't undo that
	return nil
}

func runReplay(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flagSet("replay", stderr)
	dsn := fs.String("postgres-dsn", env("", "BEEKEEPER_POSTGRES_DSN"), "Postgres DSN; empty takes the libpq environment (PGHOST, …)")
	namespace := fs.String("namespace", env("", "POD_NAMESPACE"), "read the ready? patterns from this namespace's Installation (optional)")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log, err := newLogger(*logLevel)
	if err != nil {
		return err
	}
	pool, err := newPool(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	hist := historypg.NewWriter(pool, clock.Real{}, log.WithName("history"), historypg.Options{})
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hist.Close(cctx)
	}()
	opts := []replay.Option{replay.OnMismatch(func(m replay.Mismatch) {
		line := fmt.Sprintf("mismatch: decision %d %s %s: recorded %q, replayed %q", m.ID, m.Point, m.Subject, m.Recorded, m.Replayed)
		if m.Err != nil {
			line += fmt.Sprintf(" (%v)", m.Err)
		}
		// Every mismatch also fails the command (below), so a lost line loses no verdict.
		_, _ = fmt.Fprintln(stdout, line)
	})}
	if *namespace != "" {
		if cfg, err := ctrl.GetConfig(); err == nil {
			scheme := runtime.NewScheme()
			_ = v1alpha1.AddToScheme(scheme)
			if c, err := client.New(cfg, client.Options{Scheme: scheme}); err == nil {
				var l v1alpha1.InstallationList
				if err := c.List(ctx, &l, client.InNamespace(*namespace)); err == nil && len(l.Items) > 0 {
					opts = append(opts, replay.WithSpec(&l.Items[0].Spec))
				}
			}
		}
	}
	res, err := replay.Replay(ctx, hist, opts...)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "decisions: %d, mismatches: %d\n", res.Decisions, len(res.Mismatches)); err != nil {
		return err
	}
	if len(res.Mismatches) > 0 {
		return fmt.Errorf("%d of %d decisions replay differently", len(res.Mismatches), res.Decisions)
	}
	return nil
}
