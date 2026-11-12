package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/giantswarm/beekeeper/internal/state"
)

// continuationPublication accepts only the repair targets established by the
// start gate's recorded park or loop claim. A source ref or a PR in a brief is
// not a grant. Pin the source and destination together before reserving a start.
func continuationPublication(ctx context.Context, repository string, repairs []string) (*state.ExecutionPublication, error) {
	var target *state.ExecutionPublication
	for _, raw := range repairs {
		ref, ok := parseRef(raw)
		if !ok || !strings.EqualFold(ref.Repo, repository) {
			continue
		}
		if target != nil {
			return nil, refused("continuation names multiple pull requests in its repository")
		}
		data, err := executionPublishGH(ctx, "api", fmt.Sprintf("repos/%s/pulls/%d", repository, ref.N))
		if err != nil {
			return nil, fmt.Errorf("read continuation pull request: %w", err)
		}
		var pull struct {
			State string `json:"state"`
			URL   string `json:"html_url"`
			Head  struct {
				Ref  string `json:"ref"`
				SHA  string `json:"sha"`
				Repo struct {
					FullName      string `json:"full_name"`
					DefaultBranch string `json:"default_branch"`
				} `json:"repo"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
		}
		if json.Unmarshal(data, &pull) != nil || pull.State != noteStatusOpen ||
			pull.URL != fmt.Sprintf("https://github.com/%s/pull/%d", repository, ref.N) ||
			!strings.EqualFold(pull.Head.Repo.FullName, repository) ||
			!publicationCommit(pull.Head.SHA) || pull.Head.Ref == "" || pull.Head.Ref == pull.Base.Ref ||
			pull.Head.Ref == pull.Head.Repo.DefaultBranch {
			return nil, refused("continuation requires an open pull request on a distinct branch in its repository")
		}
		target = &state.ExecutionPublication{Branch: pull.Head.Ref, Head: pull.Head.SHA, URL: pull.URL}
	}
	return target, nil
}

// closeDuplicateExecutionPulls retires publications left by older continuations
// once their recorded task's PR has merged. Only exact, nonempty task identities
// in the same repository qualify; a shared title or issue mention does not.
func closeDuplicateExecutionPulls(ctx context.Context, st *state.State, mergedURL string) error {
	var repository, task string
	for _, start := range st.Starts {
		r := start.Execution
		if r != nil && r.Published != nil && r.Published.URL == mergedURL {
			repository, task = r.Request.Repository, r.Request.Task
			break
		}
	}
	if task == "" {
		return nil
	}
	seen := map[string]bool{mergedURL: true}
	for _, start := range st.Starts {
		r := start.Execution
		if r == nil || r.Published == nil || r.Request.Repository != repository || r.Request.Task != task {
			continue
		}
		url := r.Published.URL
		if url == "" || seen[url] {
			continue
		}
		seen[url] = true
		pulls, err := ownedPulls(ctx, repository, r.Published.Branch)
		if err != nil {
			return err
		}
		if len(pulls) != 1 || pulls[0].URL != url || pulls[0].State != emergencyOpenState {
			continue
		}
		if _, err := executionPublishGH(ctx, "pr", "close", url, "--repo", repository, "--comment", "Superseded by the merged continuation "+mergedURL+"."); err != nil {
			return err
		}
	}
	return nil
}
