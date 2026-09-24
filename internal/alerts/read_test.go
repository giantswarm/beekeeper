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
// $FAKE_FAIL. It starts a child like a real port-forward's helpers and
// records both PIDs, so the test can tell whether the process group ended.
const fakeKubectl = `#!/bin/sh
case "$*" in *"config get-contexts"*) echo teleport.giantswarm.io-alpha; exit 0;; esac
if [ -n "$FAKE_FAIL" ]; then echo "$FAKE_FAIL" >&2; exit 1; fi
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
	mu     sync.Mutex
	seen   []string // path and tenant of each request
	hang   bool
}

func newFake(t *testing.T) *fake {
	dir := t.TempDir()
	kubectl := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(kubectl, []byte(fakeKubectl), 0o700); err != nil { //nolint:gosec // an executable test double
		t.Fatal(err)
	}
	f := &fake{t: t, reader: Reader{Kubectl: kubectl, Timeout: 20 * time.Second}, pids: filepath.Join(dir, "pids")}
	t.Setenv("FAKE_PIDS", f.pids)
	t.Setenv("FAKE_MIMIR", "")
	t.Setenv("FAKE_PLAIN", "")
	t.Setenv("FAKE_FAIL", "")
	return f
}

// serve starts an Alertmanager and points the service's forward at it.
func (f *fake) serve(env string) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.URL.RequestURI()+" "+r.Header.Get("X-Scope-OrgID"))
		f.mu.Unlock()
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
	if want := "/alertmanager/api/v2/alerts?" + query + " giantswarm"; len(f.seen) != 1 || f.seen[0] != want {
		t.Errorf("requests = %q, want %q", f.seen, want)
	}
	f.noForwardLeft()
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
	t.Setenv("FAKE_FAIL", "error: connection refused")
	got := f.reader.Read(context.Background(), []Target{alpha, {Name: instC}})
	if got[0].OK || got[0].Why != "error: connection refused" {
		t.Errorf("alpha = %+v", got[0])
	}
	if got[1].OK || got[1].Why != "no kube context for gamma" {
		t.Errorf("gamma = %+v", got[1])
	}
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
