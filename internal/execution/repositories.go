package execution

import (
	"errors"
	"strings"
)

func validateRepositoryOrgs(orgs []string) error {
	seen := map[string]bool{}
	for _, org := range orgs {
		key := strings.ToLower(org)
		if org == "." || org == ".." || !repoPattern.MatchString(org+"/repository") || strings.Contains(org, "/") || seen[key] {
			return errors.New("repositoryOrgs must contain distinct GitHub owners")
		}
		seen[key] = true
	}
	return nil
}

func repositoryAllowed(mapped map[string]string, orgs []string, repo string) bool {
	if !repoPattern.MatchString(repo) {
		return false
	}
	if _, ok := mapped[repo]; ok {
		return true
	}
	owner, _, _ := strings.Cut(repo, "/")
	for _, org := range orgs {
		if strings.EqualFold(owner, org) {
			return true
		}
	}
	return false
}

// AdmitsRepository reports whether this host can prepare the repository.
func (h Host) AdmitsRepository(repo string) bool {
	return repositoryAllowed(h.Repositories, h.RepositoryOrgs, repo)
}
