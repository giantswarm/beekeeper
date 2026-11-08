package guard

import (
	"errors"
	"testing"

	"github.com/giantswarm/beekeeper/internal/github"
)

func TestUpstreamRepositoryVisibility(t *testing.T) {
	for _, visibility := range []string{"public", "private", "internal", "unreadable"} {
		t.Run(visibility, func(t *testing.T) {
			u := Upstream{Orgs: []string{"giantswarm"}, Git: fakeGit, Visibility: github.VisibilityPolicy{
				PublicOnlyOwners: []string{"GIANTSWARM"}, Read: func(string) (string, error) {
					if visibility == "unreadable" {
						return "", errors.New("unreadable")
					}
					return visibility, nil
				},
			}}
			for _, command := range []string{
				"gh issue create --repo giantswarm/beekeeper --title test",
				"gh api -X POST repos/giantswarm/beekeeper/issues",
				"curl -X POST https://api.github.com/repos/giantswarm/beekeeper/issues",
				"git push origin fix",
			} {
				if got := u.bashWrite(command, oursDir); (got == nil) != (visibility == "public") {
					t.Errorf("%s: refused = %v", command, got)
				}
			}
			if got := u.connectorWrite("mcp__github__issue_write", map[string]any{"owner": "giantswarm", "repo": "beekeeper"}); (got == nil) != (visibility == "public") {
				t.Errorf("connector refused = %v", got)
			}
			if u.bashWrite("gh api -X PATCH orgs/giantswarm", oursDir) == nil {
				t.Error("unresolved repository allowed")
			}
			if u.bashWrite(`gh api graphql -f 'query=mutation { createIssue(input:{owner:"giantswarm"}) { id } }'`, oursDir) == nil {
				t.Error("opaque GraphQL mutation allowed")
			}
		})
	}
}

func TestVisibilityPolicyWithoutOwnerAllowlist(t *testing.T) {
	u := Upstream{Visibility: github.VisibilityPolicy{PublicOnlyOwners: []string{"team"}}}
	if u.bashWrite("gh issue create --repo team/private --title test", "") == nil {
		t.Fatal("empty owner allowlist disabled visibility policy")
	}
	if u.bashWrite("gh issue create --repo other/repo --title test", "") != nil {
		t.Fatal("unrestricted owner refused")
	}
}
