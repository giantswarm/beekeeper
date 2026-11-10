package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

var budgetCredentials = github.BudgetCredentials

const unlabelledIdentity = "unlabelled"

// Exact REST accounting is reserved for background inventory batches.
type freshLoopRESTKey struct{}

// Gate reads reserve time for a direct probe when another producer is busy.
type budgetGateKey struct{}
type budgetDirectKey struct{}

func (a *app) probeIdentities(parentCtx context.Context) (github.Budget, error) {
	ctx, cancel := context.WithTimeout(parentCtx, 5*time.Second)
	defer cancel()
	// Every producer (watch, loop and CLI) shares this lock. Hold it from
	// the initial cache read through publication so a delayed high response
	// cannot overwrite a concurrent low reading or return it to admission.
	lock := flock.New(filepath.Join(a.store.Dir(), "budget-probe.lock"))
	lockCtx := ctx
	gate, _ := ctx.Value(budgetGateKey{}).(bool)
	if gate {
		var stop context.CancelFunc
		lockCtx, stop = context.WithTimeout(ctx, 500*time.Millisecond)
		defer stop()
	}
	held, err := lock.TryLockContext(lockCtx, 10*time.Millisecond)
	if gate && !held && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		b, probeErr := a.readIdentityProfiles(context.WithValue(ctx, budgetDirectKey{}, true), parentCtx, false)
		b.Warnings = append(b.Warnings, "budget probe lock timed out; fallback: direct GitHub REST and GraphQL budget probes (not saved)")
		return b, probeErr
	}
	if err != nil {
		return github.Budget{}, err
	}
	if !held {
		return github.Budget{}, errors.New("budget probe lock unavailable")
	}
	defer func() { _ = lock.Unlock() }()
	return a.readIdentityProfiles(ctx, parentCtx, true)
}

func (a *app) readIdentityProfiles(ctx, parentCtx context.Context, publish bool) (github.Budget, error) {
	st, err := a.store.Peek()
	if err != nil {
		return github.Budget{}, err
	}
	now := time.Now().UTC()
	profiles := map[string]github.Budget{}
	saved := map[string]state.Budget{}
	var failures []error
	var warnings []string
	credentials := budgetCredentials(ctx)
	seen := map[string]bool{}
	for _, credential := range credentials {
		seen[credential.Identity] = true
	}
	if st.Budget != nil {
		for id, old := range st.Budget.Identities {
			if id == unlabelledIdentity {
				if old.GraphQL != nil && now.Before(old.GraphQL.Reset) {
					profiles[id], saved[id] = historicalBudget(old.GraphQL), old
				}
				continue
			}
			if !seen[id] {
				credentials = append(credentials, github.BudgetCredential{Identity: id, Err: errors.New("previously configured credential is unavailable")})
			}
		}
	}
	for _, credential := range credentials {
		var previous state.Budget
		if st.Budget != nil {
			previous = st.Budget.Identities[credential.Identity]
		}
		if credential.Skip {
			if credential.Warning != "" {
				warnings = append(warnings, credential.Warning)
			}
			// Losing provider metadata must not erase a known low unexpired window.
			// Failed metadata readings have no usable counter and are discarded.
			cached := github.Budget{Identity: credential.Identity, Login: previous.Login,
				Remaining: previous.Limit, Limit: previous.Limit, Reset: previous.Reset}
			restFresh := now.Before(previous.Reset)
			if restFresh {
				cached.Remaining = previous.Remaining
			}
			if g := previous.GraphQL; g != nil && g.Err == "" && now.Before(g.Reset) {
				cached.GraphQL = graphqlOf(g)
			}
			if restFresh || cached.GraphQL != nil {
				profiles[credential.Identity], saved[credential.Identity] = cached, previous
			}
			continue
		}
		b, cached, err := a.probeIdentity(ctx, credential, previous, saved[github.IdentityGH], now)
		profiles[credential.Identity], saved[credential.Identity] = b, cached
		if err != nil {
			failures = append(failures, fmt.Errorf("GitHub identity %s: %w", credential.Identity, err))
		}
	}
	if len(profiles) == 0 {
		return github.Budget{}, errors.New("no GitHub budget identities configured")
	}
	b := github.LowestBudget(profiles, now)
	if st.Budget != nil && len(st.Budget.Identities) == 0 {
		if old := st.Budget.GraphQL; old != nil && old.Err == "" && now.Before(old.Reset) && old.Remaining < b.GraphQL.Remaining {
			legacy := *old
			legacy.Identity = unlabelledIdentity
			profiles[unlabelledIdentity] = historicalBudget(&legacy)
			saved[unlabelledIdentity] = state.Budget{Identity: unlabelledIdentity, Remaining: legacy.Limit, Limit: legacy.Limit, Reset: legacy.Reset, At: legacy.At, GraphQL: &legacy}
			b = github.LowestBudget(profiles, now)
		}
	}
	b.Warnings = warnings
	if !publish {
		return b, errors.Join(failures...)
	}
	minimum := saved[b.Identity]
	if b.GraphQL != nil {
		minimum.GraphQL = storedGraphQL(b.GraphQL, saved[b.GraphQL.Identity].GraphQL.At)
	}
	minimum.Identities = saved
	// Publication waits like an ordinary state writer, bounded by the caller's
	// deadline. A short network-probe deadline must not turn routine lock
	// contention into a failed pre-install canary.
	wait := 2 * time.Minute
	if gate, _ := ctx.Value(budgetGateKey{}).(bool); gate {
		wait = 100 * time.Millisecond
	}
	if deadline, ok := parentCtx.Deadline(); ok {
		wait = min(wait, time.Until(deadline))
	}
	if err := parentCtx.Err(); err != nil {
		return b, errors.Join(append(failures, err)...)
	}
	if err := a.store.UpdateWithin(wait, func(st *state.State) ([]state.Event, error) {
		st.BudgetETag, st.Budget = minimum.ETag, &minimum
		return nil, nil
	}); err != nil {
		failures = append(failures, err)
	}
	b.Warnings = warnings
	return b, errors.Join(failures...)
}

func historicalBudget(g *state.GraphQL) github.Budget {
	return github.Budget{Identity: g.Identity, Login: g.Login, Remaining: g.Limit, Limit: g.Limit, Reset: g.Reset, GraphQL: graphqlOf(g)}
}

func (a *app) probeIdentity(ctx context.Context, credential github.BudgetCredential, previous, ghSaved state.Budget, now time.Time) (github.Budget, state.Budget, error) {
	id := credential.Identity
	unknown := func(err error) (github.Budget, state.Budget, error) {
		g := &github.GraphQL{Identity: id, Err: err.Error()}
		return github.Budget{Identity: id, GraphQL: g}, state.Budget{Identity: id, At: now, Windows: previous.Windows, GraphQL: storedGraphQL(g, now)}, err
	}
	if credential.Err != nil {
		return unknown(credential.Err)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	repo := a.cfg.GitHub.ProbeRepo
	if id == github.IdentityApp {
		repo = github.BudgetAppRepo
	}
	// GraphQL spend is sampled before and after every loop step. Those
	// forced samples must not force another REST probe for each identity.
	// Share a short REST cache across all producers, including spent windows.
	b := github.Budget{Identity: id, Resource: "core", Limit: previous.Limit,
		Remaining: previous.Remaining, Used: previous.Limit - previous.Remaining, Reset: previous.Reset}
	etag := previous.ETag
	var err error
	forceREST, _ := ctx.Value(freshLoopRESTKey{}).(bool)
	direct, _ := ctx.Value(budgetDirectKey{}).(bool)
	forceREST = forceREST || direct
	refreshREST := forceREST || previous.Limit == 0 || now.Sub(previous.At) >= 30*time.Second || !now.Before(previous.Reset)
	sharedGH := !direct && id == github.IdentityApp && previous.Login != "" && ghSaved.Login == previous.Login && ghSaved.Limit > 0 && now.Before(ghSaved.Reset)
	if sharedGH {
		b = github.Budget{Identity: id, Resource: "core", Limit: ghSaved.Limit,
			Remaining: ghSaved.Remaining, Used: ghSaved.Limit - ghSaved.Remaining, Reset: ghSaved.Reset}
		refreshREST = false
	} else if refreshREST {
		b, etag, err = github.Probe(ctx, client, credential.Value, repo, previous.ETag)
	}
	if err != nil {
		return unknown(err)
	}
	b.Identity = id
	g := previous.GraphQL
	if direct || g == nil || g.Identity != id || g.Err != "" || now.Sub(g.At) > a.cfg.Merge.BudgetFresh.Duration || !g.Reset.IsZero() && !now.Before(g.Reset) {
		reading := github.ProbeGraphQL(ctx, client, credential.Value, now)
		reading.Identity = id
		g = storedGraphQL(&reading, now)
	}
	g, windows := observedGraphQL(previous, g, now)
	b.GraphQL, b.Login = graphqlOf(g), g.Login
	b.Windows = map[string]github.GraphQL{}
	for key, window := range windows {
		b.Windows[key] = *graphqlOf(&window)
	}
	restAt := previous.At
	if sharedGH {
		restAt = ghSaved.At
	} else if refreshREST {
		restAt = now
	}
	cached := state.Budget{Identity: id, Login: b.Login, ETag: etag, At: restAt,
		Remaining: b.Remaining, Limit: b.Limit, Reset: b.Reset, GraphQL: g, Windows: windows}
	if g.Err != "" {
		return b, cached, errors.New(g.Err)
	}
	return b, cached, nil
}

// A source can report a different window before the previous one resets
// (a credential rotation or an inconsistent upstream reading). A high
// reading must not erase the low unexpired observation. Within a window,
// remaining points are monotonic downwards for admission purposes.
func observedGraphQL(previous state.Budget, current *state.GraphQL, now time.Time) (*state.GraphQL, map[string]state.GraphQL) {
	windows := map[string]state.GraphQL{}
	remember := func(g *state.GraphQL) {
		if g == nil || g.Err != "" || !now.Before(g.Reset) {
			return
		}
		key := g.Login + "@" + g.Reset.UTC().Format(time.RFC3339Nano)
		observed := *g
		observed.BlockedBy = nil
		if old, ok := windows[key]; ok && old.Remaining < g.Remaining {
			observed.Remaining, observed.At = old.Remaining, old.At
		}
		windows[key] = observed
	}
	for _, window := range previous.Windows {
		remember(&window)
	}
	if len(previous.Windows) == 0 {
		remember(previous.GraphQL)
	}
	remember(current)
	selected := *current
	if current.Err != "" {
		return &selected, windows
	}
	blocked := current.Blocks(now)
	for _, window := range windows {
		blocked = blocked || window.Blocks(now)
		if window.Remaining < selected.Remaining || window.Remaining == selected.Remaining && window.Reset.Before(selected.Reset) {
			selected = window
		}
	}
	if blocked && !selected.Blocks(now) {
		selected.BlockedBy = []string{current.Identity}
	}
	return &selected, windows
}

func storedGraphQL(g *github.GraphQL, at time.Time) *state.GraphQL {
	return &state.GraphQL{Identity: g.Identity, Login: g.Login, BlockedBy: g.BlockedBy, Remaining: g.Remaining,
		Limit: g.Limit, Reset: g.Reset, Refused: g.Refused, Secondary: g.Secondary, Err: g.Err, At: at}
}
