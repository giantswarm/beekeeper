package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

// prepareExecutionSource resolves the requested ref through the coordinator's
// GitHub transport. Persist before SSH so every recovery uses the same commit.
// basis names commits the host's workspace clone already holds: the bundle
// carries only the history beyond them, so a large repository's start ships
// what the host lacks, not its whole history.
func (a *app) prepareExecutionSource(ctx context.Context, r execution.StartRequest, basis []string) (execution.StartRequest, []byte, error) {
	root := filepath.Join(a.cfg.StateDir, "execution-sources", string(r.Ref.Execution))
	bundle := filepath.Join(root, "source.bundle")
	if r.SourceDigest != "" {
		data, err := readPublicationBundle(bundle)
		if err != nil {
			return r, nil, err
		}
		if execution.Digest(data) != r.SourceDigest {
			return r, nil, errors.New("saved execution source is corrupt")
		}
		return r, data, nil
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return r, nil, err
	}
	dir, err := os.MkdirTemp(root, "fetch-")
	if err != nil {
		return r, nil, err
	}
	repo := filepath.Join(dir, "repository.git")
	stagedBundle := filepath.Join(dir, "source.bundle")
	if _, err = publishGit(ctx, dir, "init", "--bare", repo); err != nil {
		return r, nil, err
	}
	remote := "https://github.com/" + r.Repository + ".git"
	if _, err = publishGit(ctx, repo, "fetch", "--no-tags", "--", remote, r.GitRef); err != nil {
		return r, nil, err
	}
	head, err := publishGit(ctx, repo, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return r, nil, err
	}
	if _, err = publishGit(ctx, repo, "update-ref", "refs/heads/execution-source", head); err != nil {
		return r, nil, err
	}
	prerequisites := sourcePrerequisites(ctx, repo, head, basis)
	if _, err = publishGit(ctx, repo, append([]string{gitBundle, workerGitHubCreate, stagedBundle, "refs/heads/execution-source"}, prerequisites...)...); err != nil {
		return r, nil, err
	}
	data, err := readPublicationBundle(stagedBundle)
	if err != nil {
		if len(prerequisites) > 0 && errors.Is(err, errSourceBundleLimit) {
			return r, nil, refused("commit bundle exceeds the publication transfer limit (64 MiB) even against the host's source of %s; bring that source up to date on the host (agents hosts check reports how far behind it is)", r.Repository)
		}
		return r, nil, err
	}
	originalRef := r.GitRef
	r.GitRef, r.SourceDigest = head, execution.Digest(data)
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		rec, _ := executionRecord(st, r.Ref)
		if rec == nil || rec.Phase != executionPending || rec.Request.SourceDigest != "" || rec.StopRequest != nil {
			return nil, refused("execution reservation changed while resolving source")
		}
		if err := a.refuseStopped(st, "pinning an SSH execution source"); err != nil {
			return nil, err
		}
		if err := os.Rename(stagedBundle, bundle); err != nil {
			return nil, err
		}
		if rec.Published == nil && originalRef != "" && originalRef != "HEAD" && originalRef != "main" && originalRef != "master" && !publicationCommit(originalRef) {
			rec.Published = &state.ExecutionPublication{At: a.now.UTC(), Branch: originalRef, Head: head}
		}
		rec.Request, rec.RequestDigest = r, r.Digest()
		return nil, nil
	})
	if err != nil {
		return r, nil, err
	}
	// The saved bundle is sufficient for recovery; discard only the successful
	// temporary fetch, as publication does after a completed transfer.
	_ = os.RemoveAll(dir)
	return r, data, nil
}

// sourcePrerequisites turns a host's basis into bundle exclusions: each basis
// commit the fetched history contains is history the host need not receive.
// Commits outside that history (other branches, unknown objects) are dropped.
// The pinned head itself cannot be excluded, or the bundle would be empty: the
// host holding it receives the head alone, against its parents.
func sourcePrerequisites(ctx context.Context, repo, head string, basis []string) []string {
	if len(basis) == 0 {
		return nil
	}
	args := append([]string{gitRevList, "--no-walk=unsorted", "--ignore-missing"}, basis...)
	present, err := publishGit(ctx, repo, args...)
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	exclude := func(c string) {
		if c != "" && c != head && !seen[c] {
			seen[c] = true
			out = append(out, "^"+c)
		}
	}
	for c := range strings.SplitSeq(present, "\n") {
		c = strings.TrimSpace(c)
		if c == head {
			parents, err := publishGit(ctx, repo, gitRevList, "--no-walk", "--parents", head)
			if err != nil {
				continue
			}
			for _, p := range strings.Fields(parents)[1:] {
				exclude(p)
			}
			continue
		}
		if _, err := publishGit(ctx, repo, "merge-base", "--is-ancestor", c, head); err == nil {
			exclude(c)
		}
	}
	return out
}
