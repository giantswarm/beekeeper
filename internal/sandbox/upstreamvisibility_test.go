package sandbox

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/giantswarm/beekeeper/internal/github"
)

func TestEgressRepositoryVisibility(t *testing.T) {
	for _, visibility := range []string{"public", "private", ""} {
		t.Run(visibility, func(t *testing.T) {
			e := &Egress{AllowedOrgs: []string{"team"}, Visibility: github.VisibilityPolicy{PublicOnlyOwners: []string{"TEAM"}, Read: func(string) (string, error) { return visibility, nil }}}
			for _, tc := range []struct {
				host, path string
				resolved   bool
			}{
				{upstreamAPIHost, "/repos/team/repo/issues", true},
				{upstreamUploadsHost, "/repos/team/repo/releases/1/assets", true},
				{upstreamGitHost, "/team/repo.git/git-receive-pack", true},
				{upstreamAPIHost, "/orgs/team", false},
			} {
				req := httptest.NewRequest(http.MethodPost, "https://"+tc.host+tc.path, nil)
				if err := e.githubWrite(req, tc.host); (err == nil) != (visibility == "public" && tc.resolved) {
					t.Errorf("%s: error = %v", tc.path, err)
				}
			}
		})
	}
}
