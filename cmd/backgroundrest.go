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

func (a *app) backgroundRESTContext(ctx context.Context) context.Context {
	who := envParty(os.Getenv)
	if who.Session != "" || who.HostSession != "" {
		return ctx
	}
	return github.WithRESTAdmission(ctx, a.admitBackgroundREST)
}

// All background processes reserve invocations against the latest measured
// REST headroom. This does not replace the measured loopScanREST allowance.
// Pagination and retries can spend more than one request per invocation.
type backgroundRESTSpend struct {
	Reading  time.Time      `json:"reading"`
	Identity string         `json:"identity,omitempty"`
	Login    string         `json:"login,omitempty"`
	Reserved int            `json:"reserved"`
	Reset    time.Time      `json:"reset"`
	Calls    int            `json:"calls"`
	Limit    int            `json:"limit"`
	Callers  map[string]int `json:"callers"`
}

func (a *app) admitBackgroundREST(ctx context.Context, class string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	lock := flock.New(filepath.Join(a.store.Dir(), "background-rest.lock"))
	held, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return fmt.Errorf("background REST admission lock: %w", err)
	}
	if !held {
		return fmt.Errorf("background REST admission lock busy")
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
	known := budget != nil && budget.Limit > 0 && now.Before(budget.Reset)
	if !now.Before(s.Reset) || known && (s.Identity != budget.Identity || s.Login != budget.Login || !s.Reset.Equal(budget.Reset)) || !known && !s.Reading.IsZero() {
		*s = backgroundRESTSpend{Reset: now.Add(time.Hour), Callers: map[string]int{}}
		if known {
			s.Reset, s.Identity, s.Login = budget.Reset, budget.Identity, budget.Login
		}
	}
	if s.Callers == nil {
		s.Callers = map[string]int{}
	}
	if !known {
		s.Limit = 100
		if s.Calls >= s.Limit {
			return fmt.Errorf("background REST held: unknown reading, bootstrap invocation cap %d reached, reset %s; caller %s", s.Limit, s.Reset.UTC().Format(time.RFC3339), class)
		}
	} else {
		// A newer measurement settles prior reservations, including the old
		// fixed-cap ledger. Reusing a reading never refunds an invocation.
		if budget.At.After(s.Reading) {
			s.Reading, s.Reserved = budget.At, 0
		}
		s.Limit = max(0, budget.Remaining-max(0, floor))
		if budget.Remaining <= max(0, floor) {
			return fmt.Errorf("background REST held: saved remaining %d of %d at or below floor %d, reading %s, reset %s; caller %s", budget.Remaining, budget.Limit, floor, budget.At.UTC().Format(time.RFC3339), budget.Reset.UTC().Format(time.RFC3339), class)
		}
		if s.Reserved >= s.Limit {
			return fmt.Errorf("background REST held: invocation allowance %d exhausted since reading %s (%d remaining, floor %d), reset %s; caller %s", s.Limit, budget.At.UTC().Format(time.RFC3339), budget.Remaining, floor, budget.Reset.UTC().Format(time.RFC3339), class)
		}
		s.Reserved++
	}
	s.Calls++
	s.Callers[class]++
	return nil
}
