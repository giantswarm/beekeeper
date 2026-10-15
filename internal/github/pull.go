package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// The states of a pull request.
const (
	Open   = "OPEN"
	Merged = "MERGED"
	Closed = "CLOSED"
)

// Pull is a pull request's state as the gh CLI reports it.
type Pull struct {
	State    string    `json:"state"`
	MergedAt time.Time `json:"mergedAt"`
}

// PullState reads repo#n's state with the gh CLI's login (one GraphQL
// request, retried as RunGH retries a read).
func PullState(ctx context.Context, repo string, n int) (Pull, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := RunGH(ctx, "pr", "view", strconv.Itoa(n), "--repo", repo, "--json", "state,mergedAt")
	if err != nil {
		return Pull{}, fmt.Errorf("gh pr view %d --repo %s: %w", n, repo, err)
	}
	var p Pull
	if err := json.Unmarshal(out, &p); err != nil {
		return Pull{}, fmt.Errorf("gh pr view %d --repo %s: %w", n, repo, err)
	}
	return p, nil
}
