package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
)

func shutdownStore(t *testing.T) *state.FileStore {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func fakeLoopShutdown(t *testing.T, grace time.Duration) chan int {
	t.Helper()
	exited := make(chan int, 1)
	oldGrace, oldWait, oldExit := loop.LoopShutdownGrace, loop.LoopShutdownLockWait, loop.LoopShutdownExit
	loop.LoopShutdownGrace, loop.LoopShutdownLockWait = grace, 50*time.Millisecond
	loop.LoopShutdownExit = func(code int) { exited <- code }
	t.Cleanup(func() {
		loop.LoopShutdownGrace, loop.LoopShutdownLockWait, loop.LoopShutdownExit = oldGrace, oldWait, oldExit
	})
	return exited
}

// A SIGTERM during a pass stuck under the state lock exits within the grace
// and the lock wait, well under systemd's 90 s stop timeout, and records the
// abandoned pass where the loop's own cleanup would have.
func TestLoopShutdownAbandonsAStuckPass(t *testing.T) {
	exited := fakeLoopShutdown(t, 100*time.Millisecond)
	store := shutdownStore(t)
	started := time.Now().UTC()
	if err := store.WriteFile(loop.LoopLifecycleFile, loop.LoopLifecycle{Version: "0.9.1", Started: started, At: started, Phase: loop.LoopPhaseRecover}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteFile(loop.LoopPIDFile, 4242); err != nil {
		t.Fatal(err)
	}
	held, stuck, updated := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		close(stuck)
		<-updated
	}()
	go func() {
		defer close(updated)
		_ = store.Update(func(*state.State) ([]state.Event, error) {
			close(held)
			<-stuck
			return nil, nil
		})
	}()
	<-held
	ctx, cancel := context.WithCancel(t.Context())
	var out bytes.Buffer
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		loop.BoundLoopShutdown(ctx, make(chan struct{}), store, &out)
	}()
	signaled := time.Now()
	cancel()
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("exit code %d, want 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no exit 10s after the stop signal")
	}
	if took := time.Since(signaled); took > 5*time.Second {
		t.Fatalf("exit %s after the stop signal, want within grace and lock wait", took)
	}
	<-returned
	var life loop.LoopLifecycle
	if found, err := store.ReadFile(loop.LoopLifecycleFile, &life); err != nil || !found {
		t.Fatalf("lifecycle: found %v, err %v", found, err)
	}
	if !life.Exited || life.Failure != loop.LoopFailureAbandoned || life.Phase != loop.LoopPhaseRecover || life.Version != "0.9.1" {
		t.Fatalf("lifecycle %+v, want exited with %q in %q", life, loop.LoopFailureAbandoned, loop.LoopPhaseRecover)
	}
	var pid int
	if _, err := store.ReadFile(loop.LoopPIDFile, &pid); err != nil || pid != 0 {
		t.Fatalf("PID marker %d (err %v), want 0", pid, err)
	}
	if !strings.Contains(out.String(), "abandoned") {
		t.Fatalf("output %q does not say the pass was abandoned", out.String())
	}
}

// A loop that returns within the grace exits on its own: no forced exit.
func TestLoopShutdownLeavesAReturningLoop(t *testing.T) {
	exited := fakeLoopShutdown(t, 200*time.Millisecond)
	store := shutdownStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		loop.BoundLoopShutdown(ctx, done, store, &bytes.Buffer{})
	}()
	cancel()
	close(done)
	<-returned
	select {
	case code := <-exited:
		t.Fatalf("forced exit %d of a loop that returned", code)
	case <-time.After(400 * time.Millisecond):
	}
	if found, err := store.ReadFile(loop.LoopLifecycleFile, &loop.LoopLifecycle{}); err != nil || found {
		t.Fatalf("lifecycle written for a returning loop (found %v, err %v)", found, err)
	}
}

// Without a stop signal the bound never starts.
func TestLoopShutdownWaitsForTheSignal(t *testing.T) {
	exited := fakeLoopShutdown(t, time.Millisecond)
	done := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		loop.BoundLoopShutdown(t.Context(), done, shutdownStore(t), &bytes.Buffer{})
	}()
	select {
	case code := <-exited:
		t.Fatalf("forced exit %d without a stop signal", code)
	case <-time.After(100 * time.Millisecond):
	}
	close(done)
	<-returned
}
