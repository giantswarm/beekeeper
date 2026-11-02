package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const IdentityApp = "app"
const IdentityGH = "gh"
const budgetHostFlag = "--hostname"
const budgetAuthCommand = "auth"

// BudgetCredential is consumed only by the HTTP probes. Its value never
// enters output, persisted state, an identifier or a hash.
type BudgetCredential struct {
	Identity string
	Value    string `json:"-"`
	Err      error  `json:"-"`
}

// BudgetCredentials explicitly selects the stored gh login and, when
// configured, devctl's App login. Neither route depends on cwd or GH_REPO.
func BudgetCredentials(ctx context.Context) []BudgetCredential {
	gh, err := budgetGH()
	if err != nil {
		return []BudgetCredential{{Identity: IdentityGH, Err: err}}
	}
	env := budgetEnv(os.Environ())
	personal := budgetCredential(ctx, IdentityGH, gh, []string{budgetAuthCommand, "token", budgetHostFlag, "github.com"}, env)
	result := []BudgetCredential{personal}
	devctl, err := budgetDevctl()
	if err != nil {
		return result
	}
	status := exec.CommandContext(ctx, devctl, budgetAuthCommand, "status") //nolint:gosec // fixed credential-provider command
	status.Env = env
	raw, err := status.Output()
	var metadata struct {
		GitHub struct {
			Present bool `json:"present"`
		} `json:"github"`
	}
	if err != nil || json.Unmarshal(raw, &metadata) != nil {
		return append(result, BudgetCredential{Identity: IdentityApp, Err: fmt.Errorf("devctl App credential metadata unavailable")})
	}
	if !metadata.GitHub.Present {
		return result
	}
	// devctl applies its owner routing even under auth exec. Pin the App's
	// owner explicitly so a personal checkout cannot select the gh login.
	appEnv := ghEnv(env, "giantswarm/beekeeper")
	return append(result, budgetCredential(ctx, IdentityApp, devctl,
		[]string{budgetAuthCommand, "exec", "--", gh, budgetAuthCommand, "token", budgetHostFlag, "github.com"}, appEnv))
}

// The loop service may have a smaller PATH than a worker. The sibling of
// the installed beekeeper and the gh link are also explicit App providers.
func budgetDevctl() (string, error) {
	if path, err := exec.LookPath("devctl"); err == nil {
		return path, nil
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if !filepath.IsAbs(dir) {
			continue
		}
		if path, err := filepath.EvalSymlinks(filepath.Join(dir, "gh")); err == nil && filepath.Base(path) == "devctl" {
			return path, nil
		}
	}
	if exe, err := os.Executable(); err == nil {
		path := filepath.Join(filepath.Dir(exe), "devctl")
		if found, err := exec.LookPath(path); err == nil {
			return found, nil
		}
	}
	return "", exec.ErrNotFound
}

func budgetCredential(ctx context.Context, identity, bin string, args, env []string) BudgetCredential {
	c := exec.CommandContext(ctx, bin, args...) //nolint:gosec // fixed auth commands and executable resolved on PATH
	c.Env = env
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
		case "GH_TOKEN", "GITHUB_TOKEN", "GH_REPO", "DEVCTL_AUTH_EXEC_CHAIN":
		default:
			out = append(out, v)
		}
	}
	return out
}

// Skip the devctl gh link; the personal route must reach the stored login.
func budgetGH() (string, error) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if !filepath.IsAbs(dir) {
			continue
		}
		path := filepath.Join(dir, "gh")
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || strings.TrimSuffix(filepath.Base(resolved), ".exe") == "devctl" {
			continue
		}
		if found, err := exec.LookPath(path); err == nil {
			return found, nil
		}
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
