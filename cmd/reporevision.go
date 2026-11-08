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
var readRepoPullRevision = func(ctx context.Context, repo, etag string) (string, bool, error) {
	return readRepoPullRevisionWithGH(ctx, github.RunGH, repo, etag)
}

func readRepoPullRevisionWithGH(ctx context.Context, run github.GH, repo, etag string) (string, bool, error) {
	args := []string{"api", "--include", "repos/" + repo + "/pulls?state=all&sort=updated&direction=desc&per_page=1"}
	if etag != "" {
		args = append(args, "-H", "If-None-Match: "+etag)
	}
	raw, err := run(github.ActingOn(ctx, repo), args...)
	if err != nil {
		return "", false, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), nil)
	if err != nil {
		return "", false, fmt.Errorf("repository pull revision: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNotModified:
		if etag == "" {
			return "", false, fmt.Errorf("repository pull revision: unexpected 304")
		}
		return etag, true, nil
	case http.StatusOK:
		return resp.Header.Get("ETag"), false, nil
	default:
		return "", false, fmt.Errorf("repository pull revision: HTTP %d", resp.StatusCode)
	}
}
