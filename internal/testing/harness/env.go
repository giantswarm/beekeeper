package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/giantswarm/k8senv"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/testing/pgtest"
)

// Env is what every scenario of a test binary shares (contract §3.4): a pool of k8senv API servers,
// each serving many scenarios in their own namespaces, and one Postgres server.
type Env struct {
	k8s    k8senv.Manager
	insts  []k8senv.Instance
	apis   []*apiServer
	stopPG func()
	ns     atomic.Int64
	// dataDir is this test binary's k8senv directory (instances and CRD cache), removed at the end;
	// cacheDir keeps the CRD caches across runs.
	dataDir, cacheDir string
	// mu guards every apiServer's load: picking the least loaded server compares the loads of
	// several servers, so one lock covers the comparison and the increment.
	mu sync.Mutex
}

type apiServer struct {
	cfg  *rest.Config
	kube client.Client
	load int // guarded by Env.mu
}

// EnvOptions configure the shared environment.
type EnvOptions struct {
	// Pool is the number of API servers (4–8).
	Pool int
	// DepsDir holds kine, k8s-<version>/kube-apiserver and postgres (pgtest.DepsDir by default).
	DepsDir string
	// CRDDir holds the engine's CRDs.
	CRDDir string
}

// Scheme is the engine kinds plus client-go's.
func Scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}

// ErrNoDeps means the test binaries aren't cached (run make test-deps); scenarios are skipped, never
// passed.
var ErrNoDeps = errors.New("test binaries missing")

// StartEnv starts the API server pool (CRDs installed from a cached database) and the Postgres server.
func StartEnv(ctx context.Context, o EnvOptions) (*Env, error) {
	if o.DepsDir == "" {
		o.DepsDir = pgtest.DepsDir()
	}
	if o.Pool <= 0 {
		o.Pool = 4
	}
	kine := filepath.Join(o.DepsDir, "kine")
	apiserver, err := newestAPIServer(o.DepsDir)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(kine); err != nil {
		return nil, fmt.Errorf("%w: %s (run make test-deps)", ErrNoDeps, kine)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	// Every test binary has its own k8senv directory, so concurrent runs never remove each other's
	// instances; the CRD cache is copied in and out of the shared directory.
	root := filepath.Join(cache, "beekeeper", "k8senv")
	reapRuns(root)
	e := &Env{cacheDir: root, dataDir: filepath.Join(root, fmt.Sprintf("run-%d", os.Getpid()))}
	// /tmp may be a small tmpfs: Postgres's data goes to the cache disk too (pgtest removes it).
	if os.Getenv("PGTEST_DIR") == "" {
		_ = os.Setenv("PGTEST_DIR", filepath.Join(cache, "beekeeper", "pgtest"))
	}
	if err := os.MkdirAll(e.dataDir, 0o750); err != nil {
		return nil, err
	}
	copyCRDCaches(e.cacheDir, e.dataDir)
	_ = os.RemoveAll(DumpDir())
	k8senv.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.k8s = k8senv.NewManager(
		k8senv.WithPoolSize(o.Pool),
		k8senv.WithCRDDir(o.CRDDir),
		k8senv.WithKineBinary(kine),
		k8senv.WithKubeAPIServerBinary(apiserver),
		k8senv.WithBaseDataDir(e.dataDir),
		k8senv.WithAcquireTimeout(3*time.Minute),
	)
	if err := e.k8s.Initialize(ctx); err != nil {
		e.Shutdown()
		return nil, fmt.Errorf("k8senv: %w", err)
	}
	copyCRDCaches(e.dataDir, e.cacheDir)

	pgDone := make(chan error, 1)
	go func() {
		stop, err := pgtest.Start()
		e.stopPG = stop
		pgDone <- err
	}()

	// Start the API servers at once: each cold start takes seconds.
	type acq struct {
		inst k8senv.Instance
		err  error
	}
	ch := make(chan acq, o.Pool)
	for i := 0; i < o.Pool; i++ {
		go func() {
			inst, err := e.k8s.Acquire(ctx)
			ch <- acq{inst, err}
		}()
	}
	var errs []error
	for i := 0; i < o.Pool; i++ {
		a := <-ch
		if a.err != nil {
			errs = append(errs, a.err)
			continue
		}
		e.insts = append(e.insts, a.inst)
		cfg, err := a.inst.Config()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		cfg = rest.CopyConfig(cfg)
		cfg.QPS, cfg.Burst = 1000, 2000
		kube, err := client.New(cfg, client.Options{Scheme: Scheme()})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		e.apis = append(e.apis, &apiServer{cfg: cfg, kube: kube})
	}
	if err := <-pgDone; err != nil {
		errs = append(errs, fmt.Errorf("postgres: %w", err))
	}
	if len(errs) > 0 {
		e.Shutdown()
		return nil, errors.Join(errs...)
	}
	return e, nil
}

// newestAPIServer finds the newest cached kube-apiserver.
func newestAPIServer(deps string) (string, error) {
	matches, _ := filepath.Glob(filepath.Join(deps, "k8s-*", "kube-apiserver"))
	if len(matches) == 0 {
		return "", fmt.Errorf("%w: no %s (run make test-deps)", ErrNoDeps, filepath.Join(deps, "k8s-*", "kube-apiserver"))
	}
	sort.Strings(matches)
	return matches[len(matches)-1], nil
}

// Namespace creates a fresh namespace for a scenario on the least loaded API server, and returns
// its rest config, the harness's client and a release func. Namespaces are never deleted (a deleted
// namespace stays Terminating on a bare API server); the pool goes with the test binary.
func (e *Env) Namespace(ctx context.Context, scenario string) (string, *rest.Config, client.Client, func(), error) {
	e.mu.Lock()
	var api *apiServer
	for _, a := range e.apis {
		if api == nil || a.load < api.load {
			api = a
		}
	}
	api.load++
	e.mu.Unlock()
	release := func() {
		e.mu.Lock()
		api.load--
		e.mu.Unlock()
	}
	name := strings.Trim(fmt.Sprintf("%.50s", scenario), "-")
	ns := fmt.Sprintf("%s-%d", name, e.ns.Add(1))
	if err := api.kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		release()
		return "", nil, nil, nil, fmt.Errorf("namespace %s: %w", ns, err)
	}
	return ns, rest.CopyConfig(api.cfg), api.kube, release, nil
}

// Shutdown stops the API servers and Postgres and removes k8senv's instance directories.
func (e *Env) Shutdown() {
	for _, inst := range e.insts {
		_ = inst.Release()
	}
	if e.k8s != nil {
		_ = e.k8s.Shutdown()
	}
	if e.stopPG != nil {
		e.stopPG()
	}
	if e.dataDir != "" {
		_ = os.RemoveAll(e.dataDir)
	}
}

// copyCRDCaches copies k8senv's cached CRD databases from one directory to another, skipping those
// already there (written to a temp file and renamed, so a concurrent reader never sees half a file).
func copyCRDCaches(from, to string) {
	files, _ := filepath.Glob(filepath.Join(from, "cached-*.db"))
	for _, f := range files {
		dst := filepath.Join(to, filepath.Base(f))
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		b, err := os.ReadFile(f) //nolint:gosec // G304: f is globbed from k8senv's own cache directory
		if err != nil {
			continue
		}
		tmp := dst + fmt.Sprintf(".%d.tmp", os.Getpid())
		if os.WriteFile(tmp, b, 0o600) == nil {
			_ = os.Rename(tmp, dst)
		}
	}
}

// reapRuns removes the k8senv directories of test binaries that are gone.
func reapRuns(root string) {
	dirs, _ := filepath.Glob(filepath.Join(root, "run-*"))
	for _, d := range dirs {
		pid, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(d), "run-"))
		if err != nil || pid == os.Getpid() {
			continue
		}
		if p, err := os.FindProcess(pid); err == nil && p.Signal(syscall.Signal(0)) == nil {
			continue
		}
		_ = os.RemoveAll(d)
	}
}
