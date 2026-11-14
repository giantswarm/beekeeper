package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/giantswarm/beekeeper/internal/clock"
	inboxpg "github.com/giantswarm/beekeeper/internal/inbox/pg"
	"github.com/giantswarm/beekeeper/internal/metrics"
	"github.com/giantswarm/beekeeper/internal/receiver"
)

func runReceiver(ctx context.Context, args []string, _, stderr io.Writer) error {
	fs := flagSet("receiver", stderr)
	listen := fs.String("listen-addr", ":8080", "webhook listen address (the public route)")
	metricsAddr := fs.String("metrics-addr", ":8081", "metrics, /healthz and /readyz listen address")
	path := fs.String("path", "/github/webhook", "the one path webhooks are accepted on")
	maxBody := fs.Int64("max-body-bytes", receiver.DefaultMaxBodyBytes, "largest body accepted (inclusive)")
	maxInFlight := fs.Int64("max-in-flight-bytes", receiver.DefaultInFlightBodies*receiver.DefaultMaxBodyBytes,
		"bodies held at once, by declared size; a delivery that doesn't fit is answered 503 unread (at least max-body-bytes)")
	secretFile := fs.String("webhook-secret-file", env("", "WEBHOOK_SECRET_FILE"), "the App's webhook secret")
	projects := fs.String("project-node-ids", env("", "PROJECT_NODE_IDS"), "comma-separated board project node ids whose projects_v2_item deliveries are stored")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	dsn := fs.String("postgres-dsn", env("", "BEEKEEPER_POSTGRES_DSN"), "Postgres DSN; empty takes the libpq environment (PGHOST, …)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log, err := newLogger(*logLevel)
	if err != nil {
		return err
	}
	secret, err := readFile(*secretFile, "webhook secret")
	if err != nil {
		return err
	}
	var nodeIDs []string
	for _, p := range strings.Split(*projects, ",") {
		if p = strings.TrimSpace(p); p != "" {
			nodeIDs = append(nodeIDs, p)
		}
	}
	pool, err := newPool(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	cfg := receiver.Config{Path: *path, MaxBodyBytes: *maxBody, MaxInFlightBytes: *maxInFlight, Secret: secret,
		Store: inboxpg.NewStore(pool), Deliveries: m.WebhookDeliveries, Clock: clock.Real{}, ProjectNodeIDs: nodeIDs,
		Logger: log.WithName("receiver")}
	if err := cfg.Validate(); err != nil {
		return err
	}
	hook := &http.Server{Addr: *listen, Handler: receiver.New(cfg), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	// Ready means the process serves: it doesn't depend on Postgres. While Postgres can't take a
	// delivery the receiver answers it 503 (counted unavailable, which BeekeeperReceiverUnavailable
	// alerts on, and GitHub logs as failed, which the engine reads back from the delivery log); taking
	// the pods out of the Service instead would hide that behind the gateway's own 503s.
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintln(w, "ok") } // a probe that can't read it fails anyway
	mux.HandleFunc("/healthz", ok)
	mux.HandleFunc("/readyz", ok)
	ops := &http.Server{Addr: *metricsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	errs := make(chan error, 2)
	for _, srv := range []*http.Server{hook, ops} {
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("%s: %w", srv.Addr, err)
			}
		}()
	}
	log.Info("receiving webhooks", "addr", *listen, "path", *path, "projects", nodeIDs)
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errs:
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = hook.Shutdown(sctx)
	_ = ops.Shutdown(sctx)
	return runErr
}
