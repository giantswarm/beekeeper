package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
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
// request).
func PullState(ctx context.Context, repo string, n int) (Pull, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	c := exec.CommandContext(ctx, "gh", "pr", "view", strconv.Itoa(n), "--repo", repo, "--json", "state,mergedAt") //nolint:gosec // a validated owner/repo and number
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return Pull{}, fmt.Errorf("gh pr view %d --repo %s: %v: %s", n, repo, err, strings.TrimSpace(stderr.String()))
	}
	var p Pull
	if err := json.Unmarshal(out, &p); err != nil {
		return Pull{}, fmt.Errorf("gh pr view %d --repo %s: %w", n, repo, err)
	}
	return p, nil
}
