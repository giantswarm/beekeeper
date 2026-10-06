// Package github reads the person's GitHub budget: the hourly REST core
// limit and the GraphQL limit every session, every gh call and the person's
// own developer portal login draw from.
//
// The budget is read from the X-RateLimit headers of a real request, never
// from /rate_limit: that endpoint is exempt and reported a full budget while
// real requests were already refused. The REST request is conditional (the
// ETag of the last answer), so a 304 costs nothing; the GraphQL one is a
// rateLimit query, one point.
package github

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// apiURL is GitHub's API; BEEKEEPER_GITHUB_API points the probes at
// another (a test's own server).
func apiURL() string {
	return cmp.Or(strings.TrimSuffix(os.Getenv("BEEKEEPER_GITHUB_API"), "/"), "https://api.github.com")
}

// Budget is the core rate limit as GitHub reported it, and the GraphQL
// limit beside it.
type Budget struct {
	Resource  string    `json:"resource"`
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Used      int       `json:"used"`
	Reset     time.Time `json:"reset"`
	// GraphQL is the GraphQL limit; nil when it was not read.
	GraphQL *GraphQL `json:"graphql,omitempty"`
}

// GraphQL is the GraphQL limit as GitHub reported it for a real query.
type GraphQL struct {
	Limit     int `json:"limit"`
	Remaining int `json:"remaining"`
	Used      int `json:"used"`
	// Reset is when the refusal ends: the hourly reset of a spent limit,
	// Retry-After of a secondary one; zero when GitHub named no time.
	Reset time.Time `json:"reset,omitzero"`
	// Refused is GitHub's words while it refuses GraphQL calls, empty while
	// it answers them.
	Refused string `json:"refused,omitempty"`
	// Secondary says the refusal is a secondary limit (too many concurrent
	// or too fast calls), not the hourly limit spent.
	Secondary bool `json:"secondary,omitempty"`
	// Err is why the limit could not be read.
	Err string `json:"error,omitempty"`
}

// Blocks says the refusal still holds at now: a refusal without a reset
// holds until a reading says otherwise.
func (g *GraphQL) Blocks(now time.Time) bool {
	return g != nil && g.Refused != "" && (g.Reset.IsZero() || now.Before(g.Reset))
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL()+"/repos/"+repo, nil)
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
	used, err := get("Used")
	if err != nil {
		used = limit - remaining
	}
	reset, _ := get("Reset")
	return Budget{
		Resource:  h.Get("X-RateLimit-Resource"),
		Limit:     limit,
		Remaining: remaining,
		Used:      used,
		Reset:     time.Unix(int64(reset), 0),
	}, nil
}

// rateLimitQuery asks GraphQL for its own limit, the cheapest real query.
const rateLimitQuery = `{"query":"{ rateLimit { limit remaining used resetAt } }"}`

// ProbeGraphQL reads the GraphQL limit with a real query, so a refusal shows
// even while the counter looks healthy: a secondary limit refuses with
// headroom left.
func ProbeGraphQL(ctx context.Context, client *http.Client, token string, now time.Time) GraphQL {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL()+"/graphql", strings.NewReader(rateLimitQuery))
	if err != nil {
		return GraphQL{Err: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return GraphQL{Err: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Data struct {
			RateLimit *struct {
				Limit     int       `json:"limit"`
				Remaining int       `json:"remaining"`
				Used      int       `json:"used"`
				ResetAt   time.Time `json:"resetAt"`
			} `json:"rateLimit"`
		} `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
		Message string `json:"message"`
	}
	var raw bytes.Buffer
	_, _ = raw.ReadFrom(resp.Body)
	_ = json.Unmarshal(raw.Bytes(), &body)
	h, herr := parse(resp.Header)
	g := GraphQL{Limit: h.Limit, Remaining: h.Remaining, Used: h.Used, Reset: h.Reset}
	if rl := body.Data.RateLimit; rl != nil {
		g = GraphQL{Limit: rl.Limit, Remaining: rl.Remaining, Used: rl.Used, Reset: rl.ResetAt}
	}
	refusal := body.Message
	for _, e := range body.Errors {
		if e.Type == "RATE_LIMITED" || strings.Contains(strings.ToLower(e.Message), "rate limit") {
			refusal = e.Message
		}
	}
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		g.Refused = cmp.Or(refusal, "GitHub answered "+resp.Status)
	case resp.StatusCode != http.StatusOK:
		return GraphQL{Err: fmt.Sprintf("GitHub answered %s for GraphQL", resp.Status)}
	case refusal != "":
		g.Refused = refusal
	case body.Data.RateLimit == nil && herr != nil:
		return GraphQL{Err: "GitHub sent no GraphQL rate limit"}
	case g.Limit > 0 && g.Remaining == 0:
		g.Refused = "the GraphQL limit is spent"
	}
	if g.Refused == "" {
		return g
	}
	if retry, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		g.Secondary, g.Reset = true, now.Add(time.Duration(retry)*time.Second)
	} else if g.Remaining > 0 || strings.Contains(strings.ToLower(g.Refused), "secondary") {
		g.Secondary, g.Reset = true, time.Time{}
	}
	return g
}
