package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/engine"
	"github.com/giantswarm/beekeeper/internal/forge/github"
	historypg "github.com/giantswarm/beekeeper/internal/history/pg"
	inboxpg "github.com/giantswarm/beekeeper/internal/inbox/pg"
	"github.com/giantswarm/beekeeper/internal/jev"
	"github.com/giantswarm/beekeeper/internal/metrics"
	"github.com/giantswarm/beekeeper/pkg/project"
)

func runEngine(ctx context.Context, args []string, _, stderr io.Writer) error {
	fs := flagSet("engine", stderr)
	namespace := fs.String("namespace", env("", "POD_NAMESPACE"), "namespace of the engine's objects (env POD_NAMESPACE)")
	replica := fs.String("replica", env("engine", "POD_NAME"), "this replica's name, the pod name (env POD_NAME)")
	metricsAddr := fs.String("metrics-addr", ":8080", `metrics listen address ("0" disables)`)
	healthAddr := fs.String("health-addr", ":8081", `/healthz and /readyz listen address ("0" disables)`)
	leaderElect := fs.Bool("leader-elect", true, "elect a leader among the replicas (only the leader observes and decides)")
	leaderID := fs.String("leader-election-id", engine.DefaultLeaderElectionID, "name of the leader election Lease")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	apiURL := fs.String("github-api-url", env("https://api.github.com", "GITHUB_API_URL"), "GitHub's REST API root (GraphQL at <url>/graphql)")
	appID := fs.String("github-app-id", env("", "GITHUB_APP_ID"), "the GitHub App's id (env GITHUB_APP_ID)")
	installationID := fs.String("github-installation-id", env("", "GITHUB_INSTALLATION_ID"), "the App's installation id (env GITHUB_INSTALLATION_ID)")
	keyFile := fs.String("github-private-key-file", env("", "GITHUB_PRIVATE_KEY_FILE"), "the App's private key (PEM)")
	jevKeyFile := fs.String("jev-key-file", env("", "JEV_KEY_FILE"), "the JEV (TypeSafe) API key; no JEV calls without it")
	jevURL := fs.String("jev-url", env("", "JEV_URL"), "JEV's endpoint (default: the Installation's spec.jev.url, else systemone)")
	dsn := fs.String("postgres-dsn", env("", "BEEKEEPER_POSTGRES_DSN"), "Postgres DSN; empty takes the libpq environment (PGHOST, …)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log, err := newLogger(*logLevel)
	if err != nil {
		return err
	}
	if *namespace == "" {
		return fmt.Errorf("--namespace is required")
	}
	app, err := strconv.ParseInt(*appID, 10, 64)
	if err != nil {
		return fmt.Errorf("--github-app-id: %w", err)
	}
	inst, err := strconv.ParseInt(*installationID, 10, 64)
	if err != nil {
		return fmt.Errorf("--github-installation-id: %w", err)
	}
	key, err := readFile(*keyFile, "GitHub App private key")
	if err != nil {
		return err
	}
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}

	pool, err := newPool(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	clk := clock.Real{}
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	// The Installation's budget reserve and JEV endpoint are applied when the leader sees it, and again
	// when it changes (it may not exist yet at start). Until then the defaults hold. Retries follow the
	// Installation on the engine's clock.
	gh, err := github.New(github.Config{
		BaseURL: *apiURL, AppID: app, InstallationID: inst, PrivateKeyPEM: key, Clock: clk,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		UserAgent:  fmt.Sprintf("beekeeper/%s (%s)", project.Version(), *replica),
		Reserve:    defaultReserve,
		Observer:   engine.GitHubObserver(m),
		Logger:     log.WithName("github"),
	})
	if err != nil {
		return err
	}
	hist := historypg.NewWriter(pool, clk, log.WithName("history"), historypg.Options{})
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hist.Close(cctx)
	}()
	drainer := inboxpg.NewDrainer(pool, clk, nil)
	defer drainer.Close()

	var jc jev.Client
	var live *liveJEV
	if *jevKeyFile != "" {
		jk, err := readFile(*jevKeyFile, "JEV key")
		if err != nil {
			return err
		}
		live = &liveJEV{key: string(jk), flagURL: *jevURL}
		live.configure(v1alpha1.JEVSpec{})
		jc = live
	}
	onInstallation := func(spec *v1alpha1.InstallationSpec) {
		if r, ok := any(gh).(reserveSetter); ok {
			r.SetReserve(reserveOf(spec))
		}
		if live != nil {
			live.configure(spec.JEV)
		}
	}

	eng, err := engine.New(engine.Options{
		Namespace: *namespace, Replica: *replica, Version: project.Version(), Rest: restCfg, Clock: clk,
		Forge: gh, History: hist, HistoryReader: hist, Inbox: drainer, JEV: jc, Registry: reg, Metrics: m,
		Leader: engine.LeaderElection{Enabled: *leaderElect, ID: *leaderID}, Logger: log,
		MetricsAddr: *metricsAddr, HealthAddr: *healthAddr, OnInstallation: onInstallation,
	})
	if err != nil {
		return err
	}
	log.Info("starting engine", "version", project.Version(), "gitSHA", project.GitSHA(), "namespace", *namespace, "replica", *replica)
	return eng.Start(ctx)
}

// defaultReserve is the observation reserve until the Installation sets one.
const defaultReserve = 0.2

// reserveSetter is a GitHub client whose observation reserve can change while it runs.
type reserveSetter interface {
	SetReserve(reserve float64)
}

// reserveOf is the Installation's observation reserve, or the default.
func reserveOf(spec *v1alpha1.InstallationSpec) float64 {
	if r := spec.Budget.ObservationReserve; r > 0 && r < 1 {
		return r
	}
	return defaultReserve
}

// liveJEV asks JEV at the endpoint, and with the timeout, the Installation configures now (the
// --jev-url flag overrides its endpoint).
type liveJEV struct {
	key     string
	flagURL string

	mu      sync.Mutex
	url     string
	timeout time.Duration
	c       jev.Client
}

// configure points the client at spec's endpoint and timeout, when they changed.
func (l *liveJEV) configure(spec v1alpha1.JEVSpec) {
	url := l.flagURL
	if url == "" {
		url = spec.URL
	}
	var timeout time.Duration
	if spec.Timeout != nil {
		timeout = spec.Timeout.Duration
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.c != nil && url == l.url && timeout == l.timeout {
		return
	}
	l.url, l.timeout = url, timeout
	l.c = jev.NewHTTP(jev.HTTPConfig{URL: url, Key: l.key, HTTPClient: &http.Client{}, Timeout: timeout})
}

// Ask implements jev.Client.
func (l *liveJEV) Ask(ctx context.Context, r jev.Request) (jev.Answer, error) {
	l.mu.Lock()
	c := l.c
	l.mu.Unlock()
	return c.Ask(ctx, r)
}
