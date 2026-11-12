package cmd

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"

	"github.com/giantswarm/beekeeper/internal/github"
)

// A 304 reuses an authenticated response without spending REST core budget.
// Read all states ordered by update so closing a pull invalidates the cache.
var ReadRepoPullRevision = func(ctx context.Context, repo, etag string) (string, bool, error) {
	return ReadRepoPullRevisionWithGH(ctx, github.RunGH, repo, etag)
}

func ReadRepoPullRevisionWithGH(ctx context.Context, run github.GH, repo, etag string) (string, bool, error) {
	args := []string{workerGitHubAPIGroup, "--include", "repos/" + repo + "/pulls?state=all&sort=updated&direction=desc&per_page=1"}
	if etag != "" {
		args = append(args, "-H", "If-None-Match: "+etag)
	}
	raw, runErr := run(github.ActingOn(ctx, repo), args...)
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), nil)
	if err != nil {
		if runErr != nil {
			return "", false, runErr
		}
		return "", false, fmt.Errorf("repository pull revision: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// gh exits 1 for 304 as well as failures; the HTTP response decides
	// whether the conditional read can reuse its cached value.
	if resp.StatusCode == http.StatusNotModified {
		if etag == "" {
			return "", false, fmt.Errorf("repository pull revision: unexpected 304")
		}
		return etag, true, nil
	}
	if runErr != nil {
		return "", false, runErr
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("repository pull revision: HTTP %d", resp.StatusCode)
	}
	return resp.Header.Get("ETag"), false, nil
}
