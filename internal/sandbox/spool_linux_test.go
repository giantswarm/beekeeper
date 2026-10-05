package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	broker(t, dir, func(pid int, req Request) error {
		mu.Lock()
		defer mu.Unlock()
		got, reqs = append(got, pid), append(reqs, req)
		if req.Unit == "refuse" {
			return errors.New("refused here")
		}
		return nil
	})
	want := Request{Op: OpScope, Unit: "memcap-1-000001", Slice: "memcap.slice", Max: "1G", Swap: "0"}
	if err := Ask(dir, want, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := Ask(dir, Request{Op: OpScope, Unit: "refuse"}, 5*time.Second); err == nil || err.Error() != "refused here" {
		t.Errorf("refusal: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != os.Getpid() || got[1] != os.Getpid() || reqs[0] != want {
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
	answer(path, "/proc", func(int, Request) error { called = true; return nil })
	raw, err := os.ReadFile(filepath.Join(dir, "x"+replySuffix))
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
	answer(path, "/proc", func(int, Request) error { return nil })
	if raw, _ := os.ReadFile(filepath.Join(dir, "big"+replySuffix)); !strings.Contains(string(raw), "over 4096 bytes") {
		t.Errorf("reply %s", raw)
	}
}
