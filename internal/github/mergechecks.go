package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// MergeResultGreen proves required CI on GitHub's synthetic merge commit for
// exactly baseSHA + head. PR-head checks never substitute for this evidence.
// Missing, stale, empty or unreadable evidence simply cannot avoid validation.
func MergeResultGreen(ctx context.Context, run GH, repo, base, baseSHA, head string, number int) (bool, error) {
	ctx = ActingOn(ctx, repo)
	raw, err := run(ctx, ghAPI, reposPrefix+repo+"/pulls/"+strconv.Itoa(number))
	if err != nil {
		return false, err
	}
	var pull struct {
		Merge string `json:"merge_commit_sha"`
		Head  struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := json.Unmarshal(raw, &pull); err != nil {
		return false, err
	}
	if pull.Merge == "" || pull.Head.SHA != head || pull.Base.SHA != baseSHA || pull.Base.Ref != base {
		return false, nil
	}
	raw, err = run(ctx, ghAPI, reposPrefix+repo+"/git/commits/"+pull.Merge)
	if err != nil {
		return false, err
	}
	var commit struct {
		SHA     string `json:"sha"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	if err := json.Unmarshal(raw, &commit); err != nil {
		return false, err
	}
	if commit.SHA != pull.Merge || len(commit.Parents) != 2 || commit.Parents[0].SHA != baseSHA || commit.Parents[1].SHA != head {
		return false, nil
	}
	// Include classic branch protection and effective rulesets. Keep App-bound
	// contexts bound to their integration, even if a legacy context also exists.
	raw, err = run(ctx, ghAPI, reposPrefix+repo+"/branches/"+url.PathEscape(base))
	if err != nil {
		return false, err
	}
	var branch struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
		Protection struct {
			Required struct {
				Contexts []string `json:"contexts"`
				Checks   []struct {
					Context string `json:"context"`
					AppID   int64  `json:"app_id"`
				} `json:"checks"`
			} `json:"required_status_checks"`
		} `json:"protection"`
	}
	if err := json.Unmarshal(raw, &branch); err != nil {
		return false, err
	}
	if branch.Commit.SHA != baseSHA {
		return false, nil
	}
	var required []bypassCheck
	for _, context := range branch.Protection.Required.Contexts {
		required = append(required, bypassCheck{Context: context})
	}
	for _, check := range branch.Protection.Required.Checks {
		required = append(required, bypassCheck{Context: check.Context, IntegrationID: check.AppID})
	}
	rules, err := bypassPages[bypassRule](ctx, run, reposPrefix+repo+"/rules/branches/"+url.PathEscape(base))
	if err != nil {
		return false, err
	}
	for _, rule := range rules {
		if rule.Type != reviewRequiredChecks {
			continue
		}
		var checks []bypassCheck
		if err := json.Unmarshal(rule.Parameters[reviewRequiredChecks], &checks); err != nil || checks == nil {
			return false, fmt.Errorf("required merge-result contexts are unreadable")
		}
		required = append(required, checks...)
	}
	if len(required) == 0 {
		return false, nil
	}
	for _, check := range required {
		if check.Context == "" {
			return false, fmt.Errorf("required merge-result context is empty")
		}
	}
	if err := bypassCheckResults(ctx, run, repo, pull.Merge, required); err != nil {
		return false, err
	}
	return true, nil
}
