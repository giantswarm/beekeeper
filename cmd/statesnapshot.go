package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// readSnapshot never repairs an event outbox or waits on the state writer.
// Age is diagnostic on stderr so JSON and status --bar keep their contracts.
func (a *app) readSnapshot() (*state.State, error) {
	st, err := a.store.Peek()
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(filepath.Join(a.store.Dir(), "state.json")); err == nil {
		pass := a.cfg.Supervisor.LoopInterval.Duration
		if pass <= 0 {
			pass = 30 * time.Second
		}
		if age := a.timeNow().Sub(info.ModTime()); age > pass {
			_, _ = fmt.Fprintf(os.Stderr, "state snapshot is %s old (supervisor pass %s)\n", age.Round(time.Second), pass)
		}
	}
	return st, nil
}
