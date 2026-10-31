//go:build linux

package guard

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/sandbox"
)

func TestSandboxRunReleasesEmptyScope(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := sandbox.Serve(ctx, dir, "/proc", time.Millisecond, func(_ context.Context, _ int, r sandbox.Request) (sandbox.Reply, error) {
			if r.Op == sandbox.OpScopeState {
				return sandbox.Reply{Out: "false"}, nil
			}
			return sandbox.Reply{}, nil
		}); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(func() { cancel(); wg.Wait() })
	exe := filepath.Join(t.TempDir(), "launcher")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexec sleep 2\n"), 0700); err != nil {
		t.Fatal(err)
	} //nolint:gosec // executable fixture in the test's own directory
	saved := plat.Capper
	plat.Capper = &fakeCapper{down: true}
	t.Cleanup(func() { plat.Capper = saved })
	oldPoll, oldStart := scopePoll, scopeStartWait
	scopePoll, scopeStartWait = time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { scopePoll, scopeStartWait = oldPoll, oldStart })
	o := Options{Max: "1M", SlotDir: t.TempDir(), Slots: 1, Stderr: io.Discard, Sandbox: sandbox.Capper{Dir: dir, Exe: exe}}
	if rc := Run(o, []string{"ignored"}); rc != 1 {
		t.Fatalf("sandboxed empty scope exit %d, want watchdog failure", rc)
	}
	lock, _, _ := acquire(o)
	if lock == nil {
		t.Fatal("sandboxed orphan retained slot")
	}
	_ = lock.Unlock()
}
