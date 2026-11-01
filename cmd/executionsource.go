package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

// prepareExecutionSource resolves the requested ref through the coordinator's
// GitHub transport. Persist before SSH so every recovery uses the same commit.
func (a *app) prepareExecutionSource(ctx context.Context, r execution.StartRequest) (execution.StartRequest, []byte, error) {
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
	if _, err = publishGit(ctx, repo, "bundle", "create", stagedBundle, "refs/heads/execution-source"); err != nil {
		return r, nil, err
	}
	data, err := readPublicationBundle(stagedBundle)
	if err != nil {
		return r, nil, err
	}
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
