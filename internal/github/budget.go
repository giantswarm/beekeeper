// Package github reads the person's GitHub REST budget: the hourly core
// limit every session, every devctl wait and the person's own developer
// portal login draw from.
//
// The budget is read from the X-RateLimit headers of a real request, never
// from /rate_limit: that endpoint is exempt and reported a full budget while
// real requests were already refused. The request is conditional (the ETag
// of the last answer), so a 304 costs nothing.
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Budget is the core rate limit as GitHub reported it.
type Budget struct {
	Resource  string    `json:"resource"`
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Used      int       `json:"used"`
	Reset     time.Time `json:"reset"`
}

// Token returns the token of the gh CLI's login.
func Token(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return "", fmt.Errorf("gh auth token: %w (log in with `gh auth login`)", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Probe reads the budget with a conditional GET of repo ("owner/name") and
// returns it with the ETag to send next time.
func Probe(ctx context.Context, client *http.Client, token, repo, etag string) (Budget, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repo, nil)
	if err != nil {
		return Budget{}, etag, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Budget{}, etag, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, perr := parse(resp.Header)
	switch resp.StatusCode {
	case http.StatusOK:
		etag = resp.Header.Get("ETag")
	case http.StatusNotModified:
	case http.StatusForbidden, http.StatusTooManyRequests:
		if perr == nil && b.Remaining == 0 {
			return b, etag, nil // spent: the headers are the answer
		}
		return b, etag, fmt.Errorf("GitHub answered %s for %s", resp.Status, repo)
	default:
		return b, etag, fmt.Errorf("GitHub answered %s for %s", resp.Status, repo)
	}
	return b, etag, perr
}

func parse(h http.Header) (Budget, error) {
	get := func(k string) (int, error) { return strconv.Atoi(h.Get("X-RateLimit-" + k)) }
	limit, err := get("Limit")
	if err != nil {
		return Budget{}, errors.New("GitHub sent no rate-limit headers")
	}
	remaining, _ := get("Remaining")
	used, _ := get("Used")
	reset, _ := get("Reset")
	return Budget{
		Resource:  h.Get("X-RateLimit-Resource"),
		Limit:     limit,
		Remaining: remaining,
		Used:      used,
		Reset:     time.Unix(int64(reset), 0),
	}, nil
}
