package execution

import "testing"

func TestRepositoryAdmissionPolicy(t *testing.T) {
	h := Host{Repositories: map[string]string{"explicit/repo": "/srv/source"}, RepositoryOrgs: []string{"example"}}
	for repo, want := range map[string]bool{"explicit/repo": true, "example/third": true, "EXAMPLE/other": true, "foreign/repo": false, "example/../../escape": false, "example": false} {
		if got := h.AdmitsRepository(repo); got != want {
			t.Errorf("admit %q = %v", repo, got)
		}
	}
	for _, orgs := range [][]string{{"example/example"}, {".."}, {"example", "EXAMPLE"}, {""}, {"*"}} {
		if err := validateRepositoryOrgs(orgs); err == nil {
			t.Errorf("invalid owners admitted: %v", orgs)
		}
	}
}
