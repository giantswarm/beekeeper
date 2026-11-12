package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"

	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

const backgroundRESTFile = "background-rest.json"
const backgroundRESTLimit = 2000

func (a *app) backgroundRESTContext(ctx context.Context) context.Context {
	who := envParty(os.Getenv)
	if who.Session != "" || who.HostSession != "" {
		return ctx
	}
	return github.WithRESTAdmission(ctx, a.admitBackgroundREST)
}

// This is an invocation ceiling, not a replacement for the measured scan
// allowance. The lower ceiling leaves room for pagination and budget probes.
// All background processes share it, including reads outside loopScanREST.
type backgroundRESTSpend struct {
	Reset   time.Time      `json:"reset"`
	Calls   int            `json:"calls"`
	Limit   int            `json:"limit"`
	Callers map[string]int `json:"callers"`
}

func (a *app) admitBackgroundREST(ctx context.Context, class string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	lock := flock.New(filepath.Join(a.store.Dir(), "background-rest.lock"))
	held, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return fmt.Errorf("background REST admission: %w", err)
	}
	if !held {
		return fmt.Errorf("background REST admission busy")
	}
	defer func() { _ = lock.Unlock() }()
	var spend backgroundRESTSpend
	if _, err := a.store.ReadFile(backgroundRESTFile, &spend); err != nil {
		return err
	}
	st, err := a.store.Peek()
	if err != nil {
		return err
	}
	if err := spend.admit(clk.Now(), st.Budget, a.cfg.GitHub.Floor, class); err != nil {
		return err
	}
	// Reserve before starting the request. A crash or failed call is counted,
	// and a restart cannot forget a caller that exhausted the allowance.
	return a.store.WriteFile(backgroundRESTFile, spend)
}

func (s *backgroundRESTSpend) admit(now time.Time, budget *state.Budget, floor int, class string) error {
	limit := backgroundRESTLimit
	known := budget != nil && budget.Limit > 0 && now.Before(budget.Reset)
	if known {
		limit = min(limit, (budget.Limit-1)/2)
	} else {
		// Permit bootstrap without an extra probe; unknown windows never get
		// a full allowance. Budget probes themselves do not use this gate.
		limit = 100
	}
	if !now.Before(s.Reset) {
		*s = backgroundRESTSpend{Reset: now.Add(time.Hour), Callers: map[string]int{}}
		if known {
			s.Reset = budget.Reset
		}
	}
	if s.Callers == nil {
		s.Callers = map[string]int{}
	}
	s.Limit = limit
	// When a probe first identifies the window, retain the bootstrap spend.
	// Never refund calls on identity/window changes while a ledger is live.
	if known && budget.Reset.After(s.Reset) {
		s.Reset = budget.Reset
	}
	if s.Calls >= limit || known && budget.Remaining <= max(floor, budget.Limit/2) {
		return fmt.Errorf("background REST held: %s used %d calls; all background callers used %d of %d, reset %s", class, s.Callers[class], s.Calls, limit, s.Reset.UTC().Format(time.RFC3339))
	}
	s.Calls++
	s.Callers[class]++
	return nil
}
