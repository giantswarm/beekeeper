package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// broker serves dir with h until the test ends.
func broker(t *testing.T, dir string, h Handler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := Serve(ctx, dir, "/proc", 5*time.Millisecond, h); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(func() { cancel(); wg.Wait() })
}

func TestAskAnswersAsTheHolder(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var got []int
	var reqs []Request
	broker(t, dir, func(_ context.Context, pid int, req Request) (Reply, error) {
		mu.Lock()
		defer mu.Unlock()
		got, reqs = append(got, pid), append(reqs, req)
		if req.Unit == "refuse" {
			return Reply{}, errors.New("refused here")
		}
		return Reply{}, nil
	})
	want := Request{Op: OpScope, Unit: "memcap-1-000001", Slice: "memcap.slice", Max: "1G", Swap: "0", Args: []string{"x"}}
	if err := Ask(dir, want, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := Ask(dir, Request{Op: OpScope, Unit: "refuse"}, 5*time.Second); err == nil || err.Error() != "refused here" {
		t.Errorf("refusal: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != os.Getpid() || got[1] != os.Getpid() || !reflect.DeepEqual(reqs[0], want) {
		t.Errorf("handled %v %v, want this process twice and %v", got, reqs, want)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("the spool keeps %v", left)
	}
}

func TestServeRefusesARequestNobodyHolds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x"+reqSuffix)
	if err := os.WriteFile(path, []byte(`{"op":"ping"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	answer(context.Background(), path, "/proc", func(context.Context, int, Request) (Reply, error) { called = true; return Reply{}, nil })
	raw, err := os.ReadFile(filepath.Join(dir, "x"+replySuffix)) //nolint:gosec // the test's own spool
	if err != nil || called || !strings.Contains(string(raw), "held by 0 processes") {
		t.Errorf("reply %s, %v; handler called %v", raw, err, called)
	}
}

func TestAskWithoutABroker(t *testing.T) {
	dir := t.TempDir()
	if err := Ask(dir, Request{Op: OpPing}, 50*time.Millisecond); !errors.Is(err, ErrNoBroker) {
		t.Errorf("no broker: %v", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("the spool keeps %v", left)
	}
}

func TestServeRefusesAnOversizedRequest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big"+reqSuffix)
	f, err := os.Create(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(`{"op":"` + strings.Repeat("x", maxRequest) + `"}`); err != nil {
		t.Fatal(err)
	}
	answer(context.Background(), path, "/proc", func(context.Context, int, Request) (Reply, error) { return Reply{}, nil })
	raw, _ := os.ReadFile(filepath.Join(dir, "big"+replySuffix)) //nolint:gosec // the test's own spool
	if !strings.Contains(string(raw), fmt.Sprintf("over %d bytes", maxRequest)) {
		t.Errorf("reply %s", raw)
	}
}

func TestCallAnswersOutputSideBySide(t *testing.T) {
	dir := t.TempDir()
	slow := make(chan struct{})
	broker(t, dir, func(_ context.Context, _ int, req Request) (Reply, error) {
		if req.Op == OpSecret {
			<-slow
			return Reply{Out: "equal\n", Err: warned, Code: 1}, nil
		}
		return Reply{}, nil
	})
	done := make(chan Reply)
	go func() {
		r, err := Call(dir, Request{Op: OpSecret, Args: []string{"compare", "a", "b"}}, 5*time.Second)
		if err != nil {
			t.Error(err)
		}
		done <- r
	}()
	// a ping is answered while the secret call still runs
	if err := Ask(dir, Request{Op: OpPing}, 2*time.Second); err != nil {
		t.Fatalf("ping behind a running call: %v", err)
	}
	close(slow)
	if r := <-done; r.Out != "equal\n" || r.Err != warned || r.Code != 1 {
		t.Errorf("reply %+v", r)
	}
}

func TestOriginKeepsOnlyTheNamedVariables(t *testing.T) {
	// a shell that says when it runs and stays the process (no tail exec):
	// Start returns before the child's environment is in place
	cmd := exec.Command("/bin/sh", "-c", "echo ready; /bin/sleep 5; true")
	cmd.Env = []string{"BEEKEEPER_TEST_ORIGIN=kept", "BEEKEEPER_TEST_OTHER=dropped"}
	cmd.Dir = t.TempDir()
	ready, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(ready).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	cwd, env, err := Origin("/proc", cmd.Process.Pid, []string{"BEEKEEPER_TEST_ORIGIN"})
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(cmd.Dir); cwd != want {
		t.Errorf("cwd %q, want %q", cwd, want)
	}
	if !reflect.DeepEqual(env, []string{"BEEKEEPER_TEST_ORIGIN=kept"}) {
		t.Errorf("env %q", env)
	}
}

// warned is a call's standard error in the tests.
const warned = "warned\n"

// signalWriter collects what is written and says each write on wrote.
type signalWriter struct {
	mu    sync.Mutex
	b     strings.Builder
	wrote chan struct{}
}

func (w *signalWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.b.Write(p)
	select {
	case w.wrote <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (w *signalWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func TestStreamCopiesOutputWhileTheCallRuns(t *testing.T) {
	dir := t.TempDir()
	more := make(chan struct{})
	broker(t, dir, func(_ context.Context, _ int, req Request) (Reply, error) {
		if req.Output() == nil {
			return Reply{}, errors.New("a streamed request without its output")
		}
		_, _ = fmt.Fprintln(req.Output(), "one")
		<-more
		_, _ = fmt.Fprintln(req.Output(), "two")
		return Reply{Err: warned, Code: 2}, nil
	})
	w := &signalWriter{wrote: make(chan struct{}, 1)}
	done := make(chan Reply)
	go func() {
		r, err := Stream(dir, Request{Op: OpWatch, Args: []string{"watch"}}, 0, w)
		if err != nil {
			t.Error(err)
		}
		done <- r
	}()
	select {
	case <-w.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("no output while the call runs")
	}
	if got := w.String(); got != "one\n" {
		t.Errorf("while running: %q", got)
	}
	close(more)
	if r := <-done; r.Err != warned || r.Code != 2 || r.Out != "" {
		t.Errorf("reply %+v", r)
	}
	if got := w.String(); got != "one\ntwo\n" {
		t.Errorf("streamed %q", got)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("the spool keeps %v", left)
	}
}

func TestAStreamedCallEndsWithItsRequester(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "w"+reqSuffix)
	if err := os.WriteFile(path, []byte(`{"op":"watch","stream":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// the requester holds its request open, then ends without a word
	requester := exec.Command("/bin/sh", "-c", "exec 3<"+path+"; echo ready; /bin/sleep 30") //nolint:gosec // the test's own spool
	ready, err := requester.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := requester.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(ready).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() {
		answer(context.Background(), path, "/proc", func(ctx context.Context, _ int, req Request) (Reply, error) {
			_, _ = fmt.Fprintln(req.Output(), "line")
			<-ctx.Done()
			return Reply{}, ctx.Err()
		})
		close(ended)
	}()
	time.Sleep(100 * time.Millisecond)
	_ = requester.Process.Kill()
	_ = requester.Wait()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the call outlived its requester")
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("the spool keeps %v", left)
	}
}
