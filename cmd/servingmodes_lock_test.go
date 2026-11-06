package cmd

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestServeModeStaleLockExcludesConcurrentSwitches(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &app{store: store}
	path := filepath.Join(store.Dir(), serveModeLockFile(selfHostedProv))
	for round := range 32 {
		if err := os.WriteFile(path, []byte("999999999\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		unlocks := make(chan func(), 32)
		var contenders sync.WaitGroup
		for range cap(unlocks) {
			contenders.Go(func() {
				<-start
				if unlock, err := a.takeServeModeLock(selfHostedProv); err == nil {
					unlocks <- unlock
				}
			})
		}
		close(start)
		contenders.Wait()
		close(unlocks)
		acquired := len(unlocks)
		for unlock := range unlocks {
			unlock()
		}
		if acquired != 1 {
			t.Fatalf("round %d: %d concurrent switches acquired one stale lock", round, acquired)
		}
	}
}
