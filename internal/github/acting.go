package github

import (
	"context"
	"strings"
)

// repoKey carries the repository a gh call acts on.
type repoKey struct{}

// ActingOn returns ctx with repo (owner/name) as the repository every gh
// call under it acts on: RunGH names it in GH_REPO. A gh link that picks
// its login by the repository's owner (devctl's agent link: the App for the
// owners it is installed on, the person's own login for every other) then
// acts with the login that reaches the repository, also on a call that
// names no repository itself, such as gh api graphql.
func ActingOn(ctx context.Context, repo string) context.Context {
	return context.WithValue(ctx, repoKey{}, repo)
}

// Repo is the repository ctx says gh acts on (ActingOn), "" when none.
func Repo(ctx context.Context) string {
	repo, _ := ctx.Value(repoKey{}).(string)
	return repo
}

// ghEnv is environ with GH_REPO set to repo, replacing one set; environ
// itself when repo is empty.
func ghEnv(environ []string, repo string) []string {
	if repo == "" {
		return environ
	}
	env := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "GH_REPO=") {
			env = append(env, kv)
		}
	}
	return append(env, "GH_REPO="+repo)
}
