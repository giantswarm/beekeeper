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
	"syscall"
	"testing"
	"time"
)

// fakeKubectl is a kubectl on disk that forwards svc/<service> to the local
// port in $FAKE_<SERVICE> (mimir-alertmanager is FAKE_MIMIR, the plain one
// FAKE_PLAIN), says "not found" for a service without one, and fails with
// $FAKE_FAIL, or once with $FAKE_FAIL_ONCE (the file $FAKE_FAILED records
// that it did). Every run appends its arguments to $FAKE_CALLS. It starts a child like a real port-forward's helpers and
// records both PIDs, so the test can tell whether the process group ended.
const fakeKubectl = `#!/bin/sh
echo "$*" >> "$FAKE_CALLS"
case "$*" in *"config get-contexts"*) echo teleport.giantswarm.io-alpha; exit 0;; esac
case "$*" in *"--context missing"*) echo 'error: context "missing" does not exist' >&2; exit 1;; esac
if [ -n "$FAKE_FAIL" ]; then echo "$FAKE_FAIL" >&2; exit 1; fi
if [ -n "$FAKE_FAIL_ONCE" ] && [ ! -e "$FAKE_FAILED" ]; then : > "$FAKE_FAILED"; echo "$FAKE_FAIL_ONCE" >&2; exit 1; fi
case "$*" in
  *svc/mimir-alertmanager*) port=$FAKE_MIMIR; svc=mimir-alertmanager;;
  *) port=$FAKE_PLAIN; svc=kube-prometheus-stack-alertmanager;;
esac
if [ -z "$port" ]; then echo "Error from server (NotFound): services \"$svc\" not found" >&2; exit 1; fi
sleep 300 &
echo $$ $! >> "$FAKE_PIDS"
echo "Forwarding from 127.0.0.1:$port -> 8080"
echo "Forwarding from [::1]:$port -> 8080"
wait
`

type fake struct {
	t      *testing.T
	reader Reader
	pids   string
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
	kubectl := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(kubectl, []byte(fakeKubectl), 0o700); err != nil { //nolint:gosec // an executable test double
		t.Fatal(err)
	}
	f := &fake{t: t, reader: Reader{Kubectl: kubectl, Timeout: 20 * time.Second, Tenant: "tenant-a"}, pids: filepath.Join(dir, "pids")}
	t.Setenv("FAKE_PIDS", f.pids)
	f.calls = filepath.Join(dir, "calls")
	t.Setenv("FAKE_CALLS", f.calls)
	t.Setenv("FAKE_MIMIR", "")
	t.Setenv("FAKE_PLAIN", "")
	t.Setenv("FAKE_FAIL", "")
	t.Setenv("FAKE_FAIL_ONCE", "")
	f.failed = filepath.Join(dir, "failed")
	t.Setenv("FAKE_FAILED", f.failed)
	pause := retryPause
	retryPause = 100 * time.Millisecond
	t.Cleanup(func() { retryPause = pause })
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

// noForwardLeft fails when a recorded kubectl or its child still runs.
func (f *fake) noForwardLeft() {
	f.t.Helper()
	raw, _ := os.ReadFile(f.pids)
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		f.t.Fatal("no port-forward was started")
	}
	for _, p := range fields {
		pid, _ := strconv.Atoi(p)
		// The shell's child may linger as a zombie for a moment until init reaps it.
		deadline := time.Now().Add(2 * time.Second)
		for syscall.Kill(pid, 0) == nil && !zombie(pid) {
			if time.Now().After(deadline) {
				f.t.Errorf("port-forward process %d still runs", pid)
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// forwards are the port-forwards the fake started: the PID of each
// kubectl.
func (f *fake) forwards() []int {
	f.t.Helper()
	raw, _ := os.ReadFile(f.pids)
	var out []int
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if fields := strings.Fields(l); len(fields) > 0 {
			pid, _ := strconv.Atoi(fields[0])
			out = append(out, pid)
		}
	}
	return out
}

// runs counts the fake's runs whose arguments contain word.
func (f *fake) runs(word string) int {
	raw, _ := os.ReadFile(f.calls)
	return strings.Count(string(raw), word)
}

func zombie(pid int) bool {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return true
	}
	s := string(raw)
	return strings.Contains(s[strings.LastIndex(s, ")"):], ") Z ")
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
	if got[0].OK || !strings.HasPrefix(got[0].Why, "error: connection refused (") || !strings.HasSuffix(got[0].Why, " attempts)") {
		t.Errorf("alpha = %+v", got[0])
	}
	if got[1].OK || got[1].Why != "no kube context for gamma" {
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
// the context ends during a read, and the forward's whole process group with it.
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
		t.Errorf("three ticks started %d forwards in %d kubectl runs, want 1", n, f.runs("port-forward"))
	}
	if len(f.seen) != 3 {
		t.Errorf("requests = %q, want three", f.seen)
	}
	f.reader.Close()
	f.noForwardLeft()
}

// A kept forward whose process ended is replaced by one new forward at the
// next tick.
func TestReadReplacesAForwardThatEnded(t *testing.T) {
	f := newFake(t)
	f.reader.Keep = true
	f.serve("FAKE_MIMIR")
	if got := f.reader.Read(context.Background(), []Target{alpha}); !got[0].OK {
		t.Fatalf("first tick: %+v", got[0])
	}
	first := f.forwards()[0]
	if err := syscall.Kill(-first, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	f.reader.mu.Lock()
	held := f.reader.held[ctxA]
	f.reader.mu.Unlock()
	<-held.exited
	if got := f.reader.Read(context.Background(), []Target{alpha}); !got[0].OK {
		t.Fatalf("second tick: %+v", got[0])
	}
	if n := len(f.forwards()); n != 2 {
		t.Errorf("forwards = %d, want the first and its one replacement", n)
	}
	f.reader.Close()
	f.noForwardLeft()
}

// A kept forward whose request fails while its process runs (the connection
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

// A Reader that keeps its forwards asks kubectl for the contexts again only
// once the kubeconfig changed.
func TestContextsKeptUntilTheKubeconfigChanges(t *testing.T) {
	f := newFake(t)
	f.reader.Keep = true
	kubeconfig := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(kubeconfig, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)
	for range 2 {
		if got := f.reader.Contexts(context.Background()); len(got) != 1 || got[0] != "teleport.giantswarm.io-alpha" {
			t.Fatalf("contexts = %q", got)
		}
	}
	if n := f.runs("get-contexts"); n != 1 {
		t.Errorf("kubectl asked %d times, want once", n)
	}
	if err := os.WriteFile(kubeconfig, []byte("ab"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.reader.Contexts(context.Background())
	if n := f.runs("get-contexts"); n != 2 {
		t.Errorf("kubectl asked %d times after the change, want twice", n)
	}
}

// A context the kubeconfig lacks is said at once: no attempt after the first
// finds it, and a Reader that knows the kubeconfig's contexts runs no
// kubectl for it.
func TestReadMissingContextIsNotRetried(t *testing.T) {
	f := newFake(t)
	missing := Target{Name: "missing", Context: "missing"}
	got := f.reader.Read(context.Background(), []Target{missing})
	if got[0].OK || got[0].Why != `error: context "missing" does not exist` || f.runs("port-forward") != 1 {
		t.Errorf("answer = %+v after %d forwards", got[0], f.runs("port-forward"))
	}
	f.reader.Keep = true
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "config"))
	f.reader.Contexts(context.Background())
	got = f.reader.Read(context.Background(), []Target{missing})
	if got[0].OK || got[0].Why != `context "missing" is not in the kubeconfig` || f.runs("port-forward") != 1 {
		t.Errorf("known contexts: answer = %+v after %d forwards", got[0], f.runs("port-forward"))
	}
}
