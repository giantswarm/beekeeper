//go:build linux

package alerts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fake struct {
	t      *testing.T
	reader Reader
	active []*portForward
	calls  string // the fake's runs, one line each
	failed string // the file the fake's failure once leaves
	mu     sync.Mutex
	seen   []string // path and tenant of each request
	hang   bool
	// fail is how many requests answer 503 before the next one answers.
	fail int
}

func newFake(t *testing.T) *fake {
	dir := t.TempDir()
	f := &fake{t: t, reader: Reader{Timeout: 20 * time.Second, Tenant: "tenant-a"}}
	f.reader.startForward = func(ctx context.Context, kctx string, ep endpoint) (*portForward, string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		log, _ := os.OpenFile(f.calls, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		defer log.Close()
		_, _ = log.WriteString("port-forward\n")
		if kctx == "missing" {
			return nil, `error: context "missing" does not exist`
		}
		if why := os.Getenv("FAKE_FAIL"); why != "" {
			return nil, why
		}
		if why := os.Getenv("FAKE_FAIL_ONCE"); why != "" {
			if _, err := os.Stat(f.failed); err != nil {
				_ = os.WriteFile(f.failed, nil, 0600)
				return nil, why
			}
		}
		env := "FAKE_PLAIN"
		if ep.service == "mimir-alertmanager" {
			env = "FAKE_MIMIR"
		}
		port, _ := strconv.Atoi(os.Getenv(env))
		if port == 0 {
			return nil, "service not found"
		}
		life := ctx
		if f.reader.Keep {
			life = context.WithoutCancel(ctx)
		}
		life, cancel := context.WithCancel(life)
		pf := &portForward{port: port, ep: ep, cancel: cancel, exited: make(chan struct{})}
		go func() { <-life.Done(); close(pf.exited) }()
		f.active = append(f.active, pf)
		return pf, ""
	}
	t.Cleanup(f.reader.Close)
	f.calls = filepath.Join(dir, "calls")
	t.Setenv("FAKE_CALLS", f.calls)
	t.Setenv("FAKE_MIMIR", "")
	t.Setenv("FAKE_PLAIN", "")
	t.Setenv("FAKE_FAIL", "")
	t.Setenv("FAKE_FAIL_ONCE", "")
	f.failed = filepath.Join(dir, "failed")
	t.Setenv("FAKE_FAILED", f.failed)
	quickRetry(t)
	return f
}

// serve starts an Alertmanager and points the service's forward at it.
func (f *fake) serve(env string) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.URL.RequestURI()+" "+r.Header.Get("X-Scope-OrgID"))
		fail := f.fail > 0
		if fail {
			f.fail--
		}
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if f.hang {
			<-r.Context().Done()
			return
		}
		_ = json.NewEncoder(w).Encode([]Raw{gateway})
	}))
	f.t.Cleanup(srv.Close)
	f.t.Setenv(env, srv.URL[strings.LastIndex(srv.URL, ":")+1:])
}

func (f *fake) noForwardLeft() {
	f.t.Helper()
	for _, pf := range f.active {
		if pf.running() {
			f.t.Error("forward still running")
		}
	}
}
func (f *fake) forwards() []int {
	out := make([]int, len(f.active))
	return out
}

// runs counts the fake's runs whose arguments contain word.
func (f *fake) runs(word string) int {
	raw, _ := os.ReadFile(f.calls)
	return strings.Count(string(raw), word)
}

var alpha = Target{Name: instA, Context: ctxA}

func TestReadMimirWithTenant(t *testing.T) {
	f := newFake(t)
	f.serve("FAKE_MIMIR")
	got := f.reader.Read(context.Background(), []Target{alpha})
	if !got[0].OK || got[0].Alerts[0].Fingerprint != "b6dd" {
		t.Fatalf("answer = %+v", got[0])
	}
	if want := "/alertmanager/api/v2/alerts?" + query + " tenant-a"; len(f.seen) != 1 || f.seen[0] != want {
		t.Errorf("requests = %q, want %q", f.seen, want)
	}
	f.noForwardLeft()
}

// Without a tenant, Mimir's Alertmanager is not asked: its anonymous
// tenant would answer an empty set.
func TestReadPlainAlertmanagerWithoutTenant(t *testing.T) {
	f := newFake(t)
	f.reader.Tenant, f.reader.Timeout = "", time.Second
	f.serve("FAKE_MIMIR")
	got := f.reader.Read(context.Background(), []Target{alpha})
	if got[0].OK || len(f.seen) != 0 {
		t.Fatalf("Mimir read without a tenant: %+v, requests %q", got[0], f.seen)
	}
}

func TestReadPlainAlertmanagerWhenNoMimir(t *testing.T) {
	f := newFake(t)
	f.serve("FAKE_PLAIN")
	got := f.reader.Read(context.Background(), []Target{alpha})
	if !got[0].OK {
		t.Fatalf("answer = %+v", got[0])
	}
	if want := "/api/v2/alerts?" + query + " "; len(f.seen) != 1 || f.seen[0] != want {
		t.Errorf("requests = %q, want %q", f.seen, want)
	}
	f.noForwardLeft()
}

func TestReadUnreachableIsAnAnswer(t *testing.T) {
	f := newFake(t)
	f.reader.Timeout = time.Second
	t.Setenv("FAKE_FAIL", "error: connection refused")
	got := f.reader.Read(context.Background(), []Target{alpha, {Name: instC}})
	if got[0].OK || got[0].NoContext || !strings.HasPrefix(got[0].Why, "error: connection refused (") || !strings.HasSuffix(got[0].Why, " attempts)") {
		t.Errorf("alpha = %+v", got[0])
	}
	if got[1].OK || !got[1].NoContext || got[1].Why != "no kube context for gamma" {
		t.Errorf("gamma = %+v", got[1])
	}
}

// A port-forward through a proxy fails now and then: the next attempt
// within the timeout answers.
func TestReadRetriesAFailedForward(t *testing.T) {
	f := newFake(t)
	f.serve("FAKE_MIMIR")
	t.Setenv("FAKE_FAIL_ONCE", "error: error upgrading connection: connection reset by peer")
	got := f.reader.Read(context.Background(), []Target{alpha})
	if !got[0].OK || got[0].Alerts[0].Fingerprint != gateway.Fingerprint {
		t.Fatalf("answer = %+v", got[0])
	}
	if _, err := os.Stat(f.failed); err != nil {
		t.Errorf("the first attempt did not fail: %v", err)
	}
	f.noForwardLeft()
}

// TestReadCancelEndsTheForward is what SIGTERM and SIGINT of a watch do:
// the context ends during a read, and the forward ends with it.
func TestReadCancelEndsTheForward(t *testing.T) {
	f := newFake(t)
	f.hang = true
	f.serve("FAKE_MIMIR")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)
	start := time.Now()
	got := f.reader.Read(ctx, []Target{alpha})
	if got[0].OK || time.Since(start) > 5*time.Second {
		t.Errorf("answer = %+v after %s", got[0], time.Since(start))
	}
	f.noForwardLeft()
}

func TestReadTimeoutPerInstallation(t *testing.T) {
	f := newFake(t)
	f.hang = true
	f.serve("FAKE_MIMIR")
	f.reader.Timeout = time.Second
	got := f.reader.Read(context.Background(), []Target{alpha})
	if got[0].OK || !strings.Contains(got[0].Why, "mimir/mimir-alertmanager") {
		t.Errorf("answer = %+v", got[0])
	}
	f.noForwardLeft()
}

// A Reader that keeps its forwards reads every tick after the first through
// the first tick's forward: no kubectl runs, so no credential plugin either.
func TestReadKeepsTheForwardAcrossTicks(t *testing.T) {
	f := newFake(t)
	f.reader.Keep = true
	f.serve("FAKE_MIMIR")
	for tick := range 3 {
		got := f.reader.Read(context.Background(), []Target{alpha})
		if !got[0].OK {
			t.Fatalf("tick %d: answer = %+v", tick, got[0])
		}
	}
	if n := len(f.forwards()); n != 1 || f.runs("port-forward") != 1 {
		t.Errorf("three ticks started %d forwards in %d starts, want 1", n, f.runs("port-forward"))
	}
	if len(f.seen) != 3 {
		t.Errorf("requests = %q, want three", f.seen)
	}
	f.reader.Close()
	f.noForwardLeft()
}

// A kept forward that ended is replaced by one new forward at the
// next tick.
func TestReadReplacesAForwardThatEnded(t *testing.T) {
	f := newFake(t)
	f.reader.Keep = true
	f.serve("FAKE_MIMIR")
	if got := f.reader.Read(context.Background(), []Target{alpha}); !got[0].OK {
		t.Fatalf("first tick: %+v", got[0])
	}
	f.reader.mu.Lock()
	held := f.reader.held[ctxA]
	f.reader.mu.Unlock()
	held.close()
	if got := f.reader.Read(context.Background(), []Target{alpha}); !got[0].OK {
		t.Fatalf("second tick: %+v", got[0])
	}
	if n := len(f.forwards()); n != 2 {
		t.Errorf("forwards = %d, want the first and its one replacement", n)
	}
	f.reader.Close()
	f.noForwardLeft()
}

// A kept forward whose request fails while it runs (the connection
// behind it dropped) is ended, and the next attempt of the same tick
// answers through one new forward.
func TestReadReplacesAForwardThatFails(t *testing.T) {
	f := newFake(t)
	f.reader.Keep = true
	f.serve("FAKE_MIMIR")
	if got := f.reader.Read(context.Background(), []Target{alpha}); !got[0].OK {
		t.Fatalf("first tick: %+v", got[0])
	}
	f.mu.Lock()
	f.fail = 1
	f.mu.Unlock()
	if got := f.reader.Read(context.Background(), []Target{alpha}); !got[0].OK {
		t.Fatalf("second tick: %+v", got[0])
	}
	if n := len(f.forwards()); n != 2 {
		t.Errorf("forwards = %d, want the first and its one replacement", n)
	}
	f.reader.Close()
	f.noForwardLeft()
}

// A Reader that keeps its forwards reloads the contexts only
// once the kubeconfig changed.
func TestContextsKeptUntilTheKubeconfigChanges(t *testing.T) {
	r := Reader{Keep: true}
	file := filepath.Join(t.TempDir(), "config")
	r.Kubeconfig = []string{file}
	write := func(name string) {
		t.Helper()
		data := "apiVersion: v1\nkind: Config\ncontexts:\n- name: " + name + "\n  context: {}\n"
		if err := os.WriteFile(file, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("first")
	for range 2 {
		if got := r.Contexts(context.Background()); len(got) != 1 || got[0] != "first" {
			t.Fatal(got)
		}
	}
	write("second")
	if got := r.Contexts(context.Background()); len(got) != 1 || got[0] != "second" {
		t.Fatal(got)
	}
}

// A context the kubeconfig lacks is said at once: no attempt after the first
// finds it, and a Reader that knows the kubeconfig's contexts runs no
// kubectl for it.
func TestReadMissingContextIsNotRetried(t *testing.T) {
	f := newFake(t)
	missing := Target{Name: "missing", Context: "missing"}
	got := f.reader.Read(context.Background(), []Target{missing})
	if got[0].OK || !got[0].NoContext || got[0].Why != `error: context "missing" does not exist` || f.runs("port-forward") != 1 {
		t.Errorf("answer = %+v after %d forwards", got[0], f.runs("port-forward"))
	}
	f.reader.Keep = true
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "config"))
	f.reader.Contexts(context.Background())
	got = f.reader.Read(context.Background(), []Target{missing})
	if got[0].OK || !got[0].NoContext || got[0].Why != `context "missing" is not in the kubeconfig` || f.runs("port-forward") != 1 {
		t.Errorf("known contexts: answer = %+v after %d forwards", got[0], f.runs("port-forward"))
	}
}

// A Reader with kubeconfig files of its own resolves contexts and forwards
// through them, whatever KUBECONFIG the process has, and notices a change of
// them.
func TestReadUsesItsOwnKubeconfig(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(file, []byte("apiVersion: v1\nkind: Config\ncontexts:\n- name: own\n  context: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "absent"))
	r := Reader{Kubeconfig: []string{file}}
	if got := r.Contexts(context.Background()); len(got) != 1 || got[0] != "own" {
		t.Fatal(got)
	}
}

func TestReadClosesRemovedTargets(t *testing.T) {
	alpha := alpha
	f := newFake(t)
	f.reader.Keep = true
	defer f.reader.Close()
	f.serve("FAKE_MIMIR")
	beta := Target{Name: instB, Context: ctxB}
	for i, ans := range f.reader.Read(context.Background(), []Target{alpha, beta}) {
		if !ans.OK {
			t.Fatalf("initial target %d failed: %s", i, ans.Why)
		}
	}
	active := f.reader.held[ctxA]
	removed := f.reader.held[ctxB]
	if removed == nil {
		t.Fatal("beta forward missing before removal")
	}
	if ans := f.reader.Read(context.Background(), []Target{alpha})[0]; !ans.OK {
		t.Fatal(ans.Why)
	}
	if removed.running() {
		t.Error("removed beta still has a running port-forward")
	}
	if len(f.reader.held) != 1 {
		t.Errorf("kept %d forwards for one target", len(f.reader.held))
	}
	if f.reader.held[ctxA] != active || f.runs("port-forward") != 2 {
		t.Error("the active target did not reuse its forward")
	}
	// Remapping the remaining installation releases its old context too.
	alpha.Context = "replacement-context"
	if ans := f.reader.Read(context.Background(), []Target{alpha})[0]; !ans.OK {
		t.Fatal(ans.Why)
	}
	if active.running() || f.runs("port-forward") != 3 {
		t.Error("replacing a target's context did not replace its forward once")
	}
	f.reader.Read(context.Background(), nil)
	if len(f.reader.held) != 0 {
		t.Error("an empty target list kept forwards")
	}
	f.noForwardLeft()
}
