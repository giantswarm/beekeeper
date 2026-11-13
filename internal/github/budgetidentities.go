package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/proc"
)

const IdentityApp = "app"
const IdentityGH = "gh"

// DevctlSkipVersionCheckEnv is the devctl-owned environment variable that
// disables devctl's interactive self-update and version gate checks when set
// to "true".
const DevctlSkipVersionCheckEnv = "DEVCTL_UNSAFE_SKIP_VERSION_CHECK"

var devctlSkipVersionCheck atomic.Bool

// SetDevctlSkipVersionCheck records whether beekeeper-owned devctl commands
// should opt out of devctl's interactive self-update and version gate checks
// (active only while hooks.guards is false).
func SetDevctlSkipVersionCheck(skip bool) {
	devctlSkipVersionCheck.Store(skip)
}

// DevctlSkipVersionCheck reports whether hooks.guards is false for
// beekeeper-owned devctl child commands.
func DevctlSkipVersionCheck() bool {
	return devctlSkipVersionCheck.Load()
}

// DevctlEnv returns env with DEVCTL_UNSAFE_SKIP_VERSION_CHECK=true when skip
// is true, and with DEVCTL_UNSAFE_SKIP_VERSION_CHECK removed when skip is
// false.
func DevctlEnv(env []string, skip bool) []string {
	prefix := DevctlSkipVersionCheckEnv + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	if skip {
		out = append(out, DevctlSkipVersionCheckEnv+"=true")
	}
	return out
}

// BudgetAppRepo pins App credential acquisition and its REST probe to the same repository.
const BudgetAppRepo = "giantswarm/beekeeper"
const budgetHostFlag = "--hostname"
const budgetAuthCommand = "auth"

// BudgetCredential is consumed only by the HTTP probes. Its value never
// enters output, persisted state, an identifier or a hash.
type BudgetCredential struct {
	Identity string
	Value    string `json:"-"`
	Err      error  `json:"-"`
	// Skip omits an absent App or one whose metadata cannot be read.
	Skip    bool   `json:"-"`
	Warning string `json:"-"`
}

// Credential helpers are process-local: no token is persisted or used as a
// cache key. Serialize acquisition so concurrent loop/watch probes fork once.
var budgetCredentialCache struct {
	sync.Mutex
	key     string
	entries map[string]cachedBudgetCredential
}

type cachedBudgetCredential struct {
	credential      BudgetCredential
	readAt, retryAt time.Time
}

// budgetCredentialMaxAge reads a held credential again well before an
// expiring one (an App's token lasts an hour) is refused in a probe: a
// refused probe is an unreadable identity, and the budget fails closed.
const budgetCredentialMaxAge = 10 * time.Minute

// BudgetCredentials selects the stored gh login and devctl's App login, read
// at most once per budgetCredentialMaxAge per process. Unavailable providers
// are retried after a minute; a rejected credential is invalidated by the
// HTTP probe. Without devctl there is no App identity, as before the cache.
// A read the caller cancelled is not kept. Neither route depends on cwd.
func BudgetCredentials(ctx context.Context) []BudgetCredential {
	budgetCredentialCache.Lock()
	defer budgetCredentialCache.Unlock()
	key := strings.Join([]string{os.Getenv("PATH"), os.Getenv("HOME"),
		os.Getenv("XDG_CONFIG_HOME"), os.Getenv("GH_CONFIG_DIR"),
		APIURL(), fmt.Sprint(DevctlSkipVersionCheck())}, "\x00")
	if budgetCredentialCache.key != key {
		budgetCredentialCache.key = key
		budgetCredentialCache.entries = nil
	}
	if budgetCredentialCache.entries == nil {
		budgetCredentialCache.entries = map[string]cachedBudgetCredential{}
	}
	now := clk.Now()
	var credentials []BudgetCredential
	for _, id := range []string{IdentityGH, IdentityApp} {
		// A missing devctl is a path lookup, no process. Leaving the App out
		// lets the caller report a previously configured App as unavailable.
		if id == IdentityApp {
			if _, err := budgetDevctl(); err != nil {
				delete(budgetCredentialCache.entries, id)
				continue
			}
		}
		cached, ok := budgetCredentialCache.entries[id]
		if !ok || !now.Before(cached.retryAt) {
			read := readBudgetCredential(ctx, id)
			if ctx.Err() != nil {
				// The caller's deadline says nothing about the provider; a
				// cached failure would fail every other caller's budget closed.
				credentials = append(credentials, read)
				continue
			}
			cached = cachedBudgetCredential{credential: read, readAt: now, retryAt: now.Add(budgetCredentialMaxAge)}
			if read.Err != nil || read.Skip {
				cached.retryAt = now.Add(time.Minute)
			}
			budgetCredentialCache.entries[id] = cached
		}
		credentials = append(credentials, cached.credential)
	}
	return credentials
}

// RejectBudgetCredential expires only the rejected identity. A broken provider
// cannot cause more than one acquisition per minute, even under concurrent 401s.
func RejectBudgetCredential(identity string) {
	budgetCredentialCache.Lock()
	defer budgetCredentialCache.Unlock()
	if cached, ok := budgetCredentialCache.entries[identity]; ok {
		if retry := cached.readAt.Add(time.Minute); retry.Before(cached.retryAt) {
			cached.retryAt = retry
		}
		budgetCredentialCache.entries[identity] = cached
	}
}

func readBudgetCredential(parent context.Context, identity string) BudgetCredential {
	gh, err := budgetGH()
	if err != nil {
		return BudgetCredential{Identity: identity, Err: err}
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	env := budgetEnv(os.Environ())
	if identity == IdentityGH {
		return budgetCredential(ctx, identity, gh, []string{budgetAuthCommand, "token", budgetHostFlag, "github.com"}, env)
	}
	devctl, err := budgetDevctl()
	if err != nil {
		return BudgetCredential{Identity: identity, Skip: true}
	}
	status := proc.CommandContext(ctx, devctl, budgetAuthCommand, "status") //nolint:gosec // fixed credential-provider command
	status.Env = env
	status.WaitDelay = 100 * time.Millisecond
	// The overall exit status covers other providers too. Its stdout can
	// still contain usable GitHub metadata.
	raw, _ := status.Output()
	var metadata struct {
		GitHub struct {
			Present *bool `json:"present"`
		} `json:"github"`
	}
	if json.Unmarshal(raw, &metadata) != nil || metadata.GitHub.Present == nil {
		return BudgetCredential{Identity: identity, Skip: true,
			Warning: "devctl App credential metadata unavailable; probing stored gh identity only"}
	}
	if !*metadata.GitHub.Present {
		return BudgetCredential{Identity: identity, Skip: true}
	}
	// Pin App routing to the same repository as its budget probe.
	return budgetCredential(ctx, identity, devctl,
		[]string{budgetAuthCommand, "exec", "--", gh, budgetAuthCommand, "token", budgetHostFlag, "github.com"}, ghEnv(env, BudgetAppRepo))
}

// The loop service may have a smaller PATH than a worker. The sibling of
// the installed beekeeper and the gh link are also explicit App providers.
func budgetDevctl() (string, error) {
	if path, err := proc.LookPath("devctl"); err == nil {
		return path, nil
	}
	for _, dir := range filepath.SplitList(os.Getenv(pathEnv)) {
		if !filepath.IsAbs(dir) {
			continue
		}
		found, err := proc.LookPath(filepath.Join(dir, "gh"))
		if err != nil {
			continue
		}
		if path, err := filepath.EvalSymlinks(found); err == nil && strings.TrimSuffix(filepath.Base(path), ".exe") == "devctl" {
			return path, nil
		}
	}
	if exe, err := os.Executable(); err == nil {
		path := filepath.Join(filepath.Dir(strings.TrimSuffix(exe, " (deleted)")), "devctl")
		if found, err := proc.LookPath(path); err == nil {
			return found, nil
		}
	}
	return "", proc.ErrNotFound
}

func budgetCredential(ctx context.Context, identity, bin string, args, env []string) BudgetCredential {
	c := proc.CommandContext(ctx, bin, args...) //nolint:gosec // fixed auth commands and executable resolved on PATH
	c.Env = env
	c.WaitDelay = 100 * time.Millisecond
	raw, err := c.Output()
	if err != nil {
		return BudgetCredential{Identity: identity, Err: fmt.Errorf("%s credential unavailable: %w", identity, err)}
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return BudgetCredential{Identity: identity, Err: fmt.Errorf("%s credential unavailable", identity)}
	}
	return BudgetCredential{Identity: identity, Value: value}
}

func budgetEnv(env []string) []string {
	var out []string
	for _, v := range env {
		name, _, _ := strings.Cut(v, "=")
		switch name {
		case "GH_TOKEN", "GITHUB_TOKEN", ghRepoEnv, "DEVCTL_AUTH_EXEC_CHAIN", DevctlSkipVersionCheckEnv:
		default:
			out = append(out, v)
		}
	}
	return DevctlEnv(out, DevctlSkipVersionCheck())
}

// Skip the devctl gh link; the personal route must reach the stored login.
func budgetGH() (string, error) {
	for _, dir := range filepath.SplitList(os.Getenv(pathEnv)) {
		if !filepath.IsAbs(dir) {
			continue
		}
		found, err := proc.LookPath(filepath.Join(dir, "gh"))
		if err != nil {
			continue
		}
		resolved, err := filepath.EvalSymlinks(found)
		if err != nil || strings.TrimSuffix(filepath.Base(resolved), ".exe") == "devctl" {
			continue
		}
		return found, nil
	}
	return "", fmt.Errorf("gh executable not found (devctl links skipped)")
}

// LowestBudget preserves each labelled reading, selecting REST and GraphQL
// independently. An unreadable identity takes precedence and fails closed.
func LowestBudget(profiles map[string]Budget, now time.Time) Budget {
	var result Budget
	for _, b := range profiles {
		if result.Identity == "" || b.Remaining < result.Remaining || b.Remaining == result.Remaining && b.Identity < result.Identity {
			g := result.GraphQL
			result = b
			result.GraphQL = g
		}
		g := b.GraphQL
		if g != nil && lowerGraphQL(g, result.GraphQL) {
			result.GraphQL = g
		}
	}
	result.Identities = profiles
	if result.GraphQL != nil {
		minimum := *result.GraphQL
		for id, profile := range profiles {
			if profile.GraphQL.Blocks(now) {
				minimum.BlockedBy = append(minimum.BlockedBy, id)
			}
		}
		slices.Sort(minimum.BlockedBy)
		result.GraphQL = &minimum
	}
	return result
}

func lowerGraphQL(g, old *GraphQL) bool {
	if old == nil {
		return true
	}
	if (g.Err != "") != (old.Err != "") {
		return g.Err != ""
	}
	return g.Remaining < old.Remaining || g.Remaining == old.Remaining && g.Identity < old.Identity
}
