package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/controller"
	"github.com/giantswarm/beekeeper/internal/metrics"
	"github.com/giantswarm/beekeeper/internal/replay"
)

// DefaultLeaderElectionID names the Lease the replicas elect a leader with.
const DefaultLeaderElectionID = "beekeeper-engine"

// Engine is one replica: a controller-runtime manager whose cache covers the engine's namespace, the
// reconcilers and the leader's observation loop. Only the leader drains the inbox, observes, schedules
// and decides; every replica serves metrics.
type Engine struct {
	opts    Options
	mgr     manager.Manager
	rt      *controller.Runtime
	metrics *metrics.Metrics
}

var _ Stats = (*Engine)(nil)

// New builds a replica from opts. It starts nothing: Start runs it.
func New(opts Options) (*Engine, error) {
	switch {
	case opts.Namespace == "":
		return nil, errors.New("engine: a namespace is required")
	case opts.Rest == nil:
		return nil, errors.New("engine: a Kubernetes REST config is required")
	case opts.Clock == nil:
		return nil, errors.New("engine: a clock is required")
	case opts.Forge == nil:
		return nil, errors.New("engine: a forge reader is required")
	}
	if opts.Replica == "" {
		opts.Replica = "engine"
	}
	if opts.Registry == nil {
		opts.Registry = prometheus.NewRegistry()
	}
	m := opts.Metrics
	if m == nil {
		m = metrics.New(opts.Registry)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	health := opts.HealthAddr
	if health == "" {
		health = "0"
	}
	mopts := ctrl.Options{
		Scheme:                 scheme,
		Logger:                 opts.Logger,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: health,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{opts.Namespace: {}},
			// Every event comes from a watch; the engine's own resyncs run on its clock.
			SyncPeriod: ptr.To(10 * time.Hour),
		},
		Controller:                    config.Controller{SkipNameValidation: ptr.To(true)},
		LeaderElection:                opts.Leader.Enabled,
		LeaderElectionNamespace:       opts.Namespace,
		LeaderElectionID:              opts.Leader.ID,
		LeaderElectionReleaseOnCancel: true,
	}
	if mopts.LeaderElectionID == "" {
		mopts.LeaderElectionID = DefaultLeaderElectionID
	}
	if d := opts.Leader.LeaseDuration; d > 0 {
		mopts.LeaseDuration = &d
	}
	if d := opts.Leader.RenewDeadline; d > 0 {
		mopts.RenewDeadline = &d
	}
	if d := opts.Leader.RetryPeriod; d > 0 {
		mopts.RetryPeriod = &d
	}
	mgr, err := ctrl.NewManager(opts.Rest, mopts)
	if err != nil {
		return nil, fmt.Errorf("engine: manager: %w", err)
	}
	if health != "0" {
		if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
			return nil, err
		}
		if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
			return nil, err
		}
	}

	rt := controller.New(controller.Config{
		Namespace: opts.Namespace, Replica: opts.Replica, Clock: opts.Clock, Tracker: opts.Tracker, Forge: opts.Forge,
		History: opts.History, HistoryReader: opts.HistoryReader, Inbox: opts.Inbox, JEV: opts.JEV, Faults: opts.Faults,
		Metrics: m, Logger: opts.Logger.WithName("engine"), Workers: opts.Workers, OnInstallation: opts.OnInstallation,
	})
	if err := rt.Setup(context.Background(), mgr); err != nil {
		return nil, fmt.Errorf("engine: setup: %w", err)
	}
	m.SetSnapshot(rt.Snapshot)
	return &Engine{opts: opts, mgr: mgr, rt: rt, metrics: m}, nil
}

// Start runs the replica until ctx ends: the manager (cache, leader election, controllers) and, when
// MetricsAddr is set, the metrics endpoint. On ctx's end it stops every timer and releases the lease.
func (e *Engine) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 1)
	if addr := e.opts.MetricsAddr; addr != "" && addr != "0" {
		srv := &http.Server{Addr: addr, ReadHeaderTimeout: 10 * time.Second,
			Handler: promhttp.HandlerFor(prometheus.Gatherers{e.opts.Registry, ctrlmetrics.Registry}, promhttp.HandlerOpts{})}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("engine: metrics listener: %w", err)
		}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- err
				cancel()
			}
		}()
		go func() {
			<-ctx.Done()
			sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer scancel()
			_ = srv.Shutdown(sctx)
		}()
	}
	err := e.mgr.Start(ctx)
	select {
	case serr := <-errs:
		return errors.Join(err, serr)
	default:
	}
	return err
}

// Reconciles implements Stats.
func (e *Engine) Reconciles(kind, ref string) int { return e.rt.Reconciles(kind, ref) }

// Writes implements Stats.
func (e *Engine) Writes() int { return e.rt.Writes() }

// IsLeader implements Stats.
func (e *Engine) IsLeader() bool { return e.rt.IsLeader() }

// Metrics are the replica's metrics.
func (e *Engine) Metrics() *metrics.Metrics { return e.metrics }

// Runtime is the replica's controllers and observation state (tests).
func (e *Engine) Runtime() *controller.Runtime { return e.rt }

// Replay re-runs every recorded decision's rule from its recorded inputs, with the Installation's
// ready? patterns, and returns how many decisions there were and how many answer differently now.
func (e *Engine) Replay(ctx context.Context) (decisions, mismatches int, err error) {
	if e.opts.HistoryReader == nil {
		return 0, 0, errors.New("engine: no history reader")
	}
	var opts []replay.Option
	var l v1alpha1.InstallationList
	if err := e.mgr.GetAPIReader().List(ctx, &l, client.InNamespace(e.opts.Namespace)); err == nil && len(l.Items) > 0 {
		sort.Slice(l.Items, func(i, j int) bool { return l.Items[i].Name < l.Items[j].Name })
		opts = append(opts, replay.WithSpec(&l.Items[0].Spec))
	}
	return replay.Run(ctx, e.opts.HistoryReader, opts...)
}
