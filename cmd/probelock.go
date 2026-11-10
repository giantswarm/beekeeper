package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/gofrs/flock"
)

// Probes never queue behind another probe or hold the state lock. A contender
// uses the last valid usage observation (including its quota stops), or refuses
// an unreadable provider. This also works between independent CLI processes.
func (a *app) usageRefreshLock(file string) (func(), error) {
	return a.probeLock(file + ".refresh.lock")
}

func (a *app) probeLock(name string) (func(), error) {
	if a.store == nil || a.store.Dir() == "" {
		return func() {}, nil
	}
	lock := flock.New(filepath.Join(a.store.Dir(), name))
	held, err := lock.TryLock()
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	if !held {
		_ = lock.Close()
		return nil, fmt.Errorf("probe already in progress: %s", name)
	}
	return func() { _ = lock.Unlock() }, nil
}
