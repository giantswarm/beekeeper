package harness

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/rest"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/faults"
	"github.com/giantswarm/beekeeper/internal/history"
	historypg "github.com/giantswarm/beekeeper/internal/history/pg"
	inboxpg "github.com/giantswarm/beekeeper/internal/inbox/pg"
	"github.com/giantswarm/beekeeper/internal/metrics"
	"github.com/giantswarm/beekeeper/internal/receiver"
	"github.com/giantswarm/beekeeper/internal/settle"
	fakegithub "github.com/giantswarm/beekeeper/internal/testing/fakes/github"
	fakejev "github.com/giantswarm/beekeeper/internal/testing/fakes/jev"
	"github.com/giantswarm/beekeeper/internal/testing/pgtest"
)

// Leader election in leader scenarios runs on real time, kept short (contract §3.2).
var leaderElection = struct{ lease, renew, retry time.Duration }{2 * time.Second, 1500 * time.Millisecond, 200 * time.Millisecond}

// leadTimeout bounds how long engine-0 may take to lead after it starts.
const leadTimeout = 30 * time.Second

// ReplicaDeps is what the harness gives one engine process; the engine wiring builds it into an Engine.
type ReplicaDeps struct {
	Name      string
	Namespace string
	Rest      *rest.Config
	Clock     *clock.Fake
	Tracker   *settle.Tracker
	// ForgeURL, AppID, InstallationID and AppKeyPEM reach the fake GitHub as the App.
	ForgeURL       string
	AppID          int64
	InstallationID int64
	AppKeyPEM      []byte
	UserAgent      string
	Spec           v1alpha1.InstallationSpec
	History        *historypg.Writer
	Inbox          *inboxpg.Drainer
	JEV            *fakejev.Fake
	Registry       *prometheus.Registry
	Faults         *faults.Faults
	// Leader is set with two replicas.
	Leader bool
	Logger logr.Logger
}

// buildEngine turns a process's dependencies into an engine; engine_wiring.go sets it.
var buildEngine func(d ReplicaDeps) (Engine, error)

// NewWorld builds a scenario's world on env and starts its engines; it is torn down when t ends
// (engines stopped before anything they use).
func NewWorld(t testing.TB, env *Env, sc *Scenario) (*World, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pre := sc.Pre
	spec, err := pre.Config.InstallationSpec()
	if err != nil {
		return nil, err
	}
	app, err := appOf(pre.GitHub)
	if err != nil {
		return nil, err
	}

	w := &World{
		Scenario: sc,
		Clock:    clock.NewFake(pre.Clock),
		Tracker:  settle.New(),
		Config:   pre.Config,
		Logs:     NewLogBuffer(3000),
		tracked:  trackedKeys(sc),
	}
	t0 := time.Now()
	phase := func(name string) { w.Phases = append(w.Phases, Phase{name, time.Since(t0)}) }
	logger := funcr.New(func(prefix, args string) { w.Logs.Add(prefix + " " + args) }, funcr.Options{Verbosity: 1})

	// The database first: pgtest drops it after the cleanups registered later, so the engines stop
	// before it goes.
	db := pgtest.NewDB(t)
	w.DB = db
	phase("database")
	life, stopLife := context.WithCancel(context.Background())
	w.life = life
	var closers []func()
	t.Cleanup(func() {
		for _, r := range w.Replicas {
			_ = r.Stop(w.tracked)
		}
		stopLife()
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	})

	// The API server and the scenario's namespace.
	ns, cfg, kube, release, err := env.Namespace(ctx, sc.Name)
	if err != nil {
		return w, err
	}
	closers = append(closers, release)
	w.Namespace, w.Kube = ns, kube
	phase("namespace")

	// History, read by the harness and seeded with durations.
	reader := historypg.NewWriter(db.Pool(), w.Clock, logger.WithName("harness"), historypg.Options{})
	closers = append(closers, func() { _ = reader.Close(context.Background()) })
	w.History = reader
	if err := seedHistory(ctx, reader, pre); err != nil {
		return w, err
	}
	phase("history")

	// The receiver on a loopback listener.
	recvReg := prometheus.NewRegistry()
	recvMetrics := metrics.New(recvReg)
	handler := receiver.New(receiver.Config{
		Path:           spec.Receiver.Path,
		MaxBodyBytes:   spec.Receiver.MaxBodyBytes,
		Secret:         app.secret,
		Store:          inboxpg.NewStore(db.Pool()),
		Deliveries:     recvMetrics.WebhookDeliveries,
		Clock:          w.Clock,
		ProjectNodeIDs: []string{fmt.Sprintf("PVT_%s_%d", spec.Org, spec.Board.Project)},
		Tracker:        w.Tracker,
		Logger:         logger.WithName("receiver"),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return w, err
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	closers = append(closers, func() { _ = srv.Close() })
	w.Receiver = Receiver{
		URL: "http://" + ln.Addr().String(), Path: spec.Receiver.Path, Secret: app.secret,
		Registry: recvReg, Client: &http.Client{Timeout: 30 * time.Second},
	}

	// The fakes.
	gh, err := fakegithub.New(fakegithub.Options{
		Clock: w.Clock, Tracker: w.Tracker, WebhookURL: w.Receiver.URL + spec.Receiver.Path, WebhookSecret: string(app.secret),
	})
	if err != nil {
		return w, err
	}
	closers = append(closers, gh.Close)
	if err := gh.Seed(pre.GitHub); err != nil {
		return w, err
	}
	w.GitHub = gh
	jv := fakejev.New(fakejev.Options{Clock: w.Clock, Tracker: w.Tracker})
	jevSeed := pre.JEV
	if jevSeed == nil {
		jevSeed = map[string]any{}
	}
	if err := jv.Seed(jevSeed); err != nil {
		return w, err
	}
	w.JEV = jv
	phase("fakes")

	// The configuration, as Flux applies it.
	if err := ApplyConfig(ctx, kube, ns, pre.Config); err != nil {
		return w, err
	}
	phase("configuration")

	// The replicas.
	if buildEngine == nil {
		return w, errors.New("the engine isn't wired into the harness")
	}
	two := pre.Engine.Replicas == 2
	for i := 0; i < pre.Engine.Replicas; i++ {
		name := fmt.Sprintf("engine-%d", i)
		f := &faults.Faults{}
		w.Replicas = append(w.Replicas, NewReplica(name, f, func() (Engine, *prometheus.Registry, func(), error) {
			reg := prometheus.NewRegistry()
			hist := historypg.NewWriter(db.Pool(), w.Clock, logger.WithName(name).WithName("history"), historypg.Options{Tracker: w.Tracker, RetryInterval: 100 * time.Millisecond})
			drainer := inboxpg.NewDrainer(db.Pool(), w.Clock, w.Tracker)
			eng, err := buildEngine(ReplicaDeps{
				Name: name, Namespace: ns, Rest: rest.CopyConfig(cfg), Clock: w.Clock, Tracker: w.Tracker,
				ForgeURL: gh.URL(), AppID: app.id, InstallationID: app.installation, AppKeyPEM: gh.AppKeyPEM(),
				UserAgent: fmt.Sprintf("beekeeper/test (%s)", name), Spec: spec,
				History: hist, Inbox: drainer, JEV: jv, Registry: reg, Faults: f, Leader: two,
				Logger: logger.WithName(name),
			})
			closeAll := func() {
				drainer.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = hist.Close(ctx)
			}
			if err != nil {
				closeAll()
				return nil, nil, nil, err
			}
			return eng, reg, closeAll, nil
		}))
	}
	w.startReplicas = func(ctx context.Context) error { return w.startAll(ctx, nil) }
	if err := w.startAll(ctx, phase); err != nil {
		return w, err
	}
	return w, nil
}

// startAll starts the replicas in order: engine-0 first, and engine-1 only once engine-0 leads.
func (w *World) startAll(ctx context.Context, phase func(string)) error {
	if phase == nil {
		phase = func(string) {}
	}
	for i, r := range w.Replicas {
		if err := r.Start(w.life); err != nil {
			return err
		}
		if i == 0 {
			if err := waitLeader(ctx, r); err != nil {
				return err
			}
			phase(r.Name + " leads")
		}
	}
	err := w.Settle(ctx)
	phase("settled")
	return err
}

func waitLeader(ctx context.Context, r *Replica) error {
	deadline := time.Now().Add(leadTimeout)
	for !r.IsLeader() {
		if gone, err := r.Exited(); gone {
			return fmt.Errorf("%s exited before leading: %v", r.Name, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s didn't lead within %s", r.Name, leadTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	return nil
}

// seedHistory writes the scenario's durations, oldest first, before its clock.
func seedHistory(ctx context.Context, w *historypg.Writer, pre *Pre) error {
	n := 0
	add := func(kind, key string, ds []string) {
		durs, _ := Durations(ds)
		for i, d := range durs {
			w.AddDuration(kind, key, d, pre.Clock.Add(-time.Duration(len(durs)-i)*time.Hour))
			n++
		}
	}
	for _, c := range pre.History.CI {
		add(history.DurationCI, c.Repo, c.Durations)
	}
	for _, s := range pre.History.Stages {
		add(history.DurationStage, s.Team+"/"+s.Phase, s.Durations)
	}
	if n == 0 {
		return nil
	}
	for w.Pending() > 0 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("seeding history: %w", ctx.Err())
		case <-time.After(2 * time.Millisecond):
		}
	}
	return nil
}

type appConfig struct {
	id, installation int64
	secret           []byte
}

// appOf reads the App from the fake GitHub's seed.
func appOf(gh map[string]any) (appConfig, error) {
	a, _ := gh["app"].(map[string]any)
	id, ok1 := toFloat(a["id"])
	inst, ok2 := toFloat(a["installation"])
	secret, ok3 := a["webhookSecret"].(string)
	if !ok1 || !ok2 || !ok3 || secret == "" {
		return appConfig{}, fmt.Errorf("github.app needs id, installation and webhookSecret: %v", a)
	}
	return appConfig{id: int64(id), installation: int64(inst), secret: []byte(secret)}, nil
}
