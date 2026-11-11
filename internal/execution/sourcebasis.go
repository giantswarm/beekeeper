package execution

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
)

// SourceBasisOperation asks a host which commits of a repository a new
// workspace there already holds, so the coordinator bundles only the rest.
const SourceBasisOperation = "source-basis"

// MaxSourceBasis bounds the commits one basis reply names.
const MaxSourceBasis = 256

var basisCommitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// SourceBasis lists the commits a workspace clone of repository starts with.
// The clone copies the mapped checkout's branches, so their tips (newest
// first, HEAD's commit leading) are commits whose objects the host holds. An
// unmapped repository is cloned from the bundle alone and has no basis.
type SourceBasis struct {
	Commits []string `json:"commits"`
}

// SourceBasis reads only the host's own mapped checkout; it changes nothing.
func (s *Server) SourceBasis(ctx context.Context, repository string) (SourceBasis, error) {
	if !repositoryAllowed(s.Config.Repositories, s.Config.RepositoryOrgs, repository) {
		return SourceBasis{}, errors.New("repository is not admitted by execution host policy")
	}
	source, mapped := s.Config.Repositories[repository]
	if !mapped {
		return SourceBasis{}, nil
	}
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return SourceBasis{}, errors.New("execution host checkout is unavailable")
	}
	origin, err := s.git(ctx, resolved, "remote", "get-url", "origin")
	if err != nil || RepositoryIdentity(string(origin)) != strings.ToLower(repository) {
		return SourceBasis{}, errors.New("execution host checkout origin does not match the repository allowlist")
	}
	var basis SourceBasis
	seen := map[string]bool{}
	add := func(raw string) {
		c := strings.TrimSpace(raw)
		if basisCommitPattern.MatchString(c) && !seen[c] && len(basis.Commits) < MaxSourceBasis {
			seen[c] = true
			basis.Commits = append(basis.Commits, c)
		}
	}
	// HEAD's branch is what a clone checks out; a detached HEAD is not cloned.
	if branch, err := s.git(ctx, resolved, "symbolic-ref", "-q", "HEAD"); err == nil {
		if head, err := s.git(ctx, resolved, "rev-parse", "--verify", "-q", strings.TrimSpace(string(branch))+"^{commit}"); err == nil {
			add(string(head))
		}
	}
	tips, err := s.git(ctx, resolved, "for-each-ref", "--sort=-committerdate", "--format=%(objectname)", "refs/heads")
	if err != nil {
		return SourceBasis{}, errors.New("cannot list the execution host checkout's branches")
	}
	for line := range strings.SplitSeq(string(tips), "\n") {
		add(line)
	}
	return basis, nil
}

// SourceReport is hosts check's comparison of one mapped host source with
// its repository's default branch on GitHub.
type SourceReport struct {
	Repository string `json:"repository"`
	Head       string `json:"head,omitempty"`
	Branch     string `json:"branch,omitempty"`
	// Missing counts the default branch's commits the host source lacks.
	Missing int    `json:"missingCommits"`
	Problem string `json:"problem,omitempty"`
}

// SourceBasisReader is a transport that can ask a host for its source basis.
type SourceBasisReader interface {
	SourceBasis(context.Context, Ref, string) (SourceBasis, error)
}

func (t SSHTransport) SourceBasis(ctx context.Context, ref Ref, repository string) (SourceBasis, error) {
	var basis SourceBasis
	if err := t.Call(ctx, SourceBasisOperation, ref, repository, &basis); err != nil {
		return SourceBasis{}, err
	}
	if len(basis.Commits) > MaxSourceBasis {
		return SourceBasis{}, errors.New("source basis exceeds its bound")
	}
	for _, c := range basis.Commits {
		if !basisCommitPattern.MatchString(c) {
			return SourceBasis{}, errors.New("source basis names an invalid commit")
		}
	}
	return basis, nil
}
