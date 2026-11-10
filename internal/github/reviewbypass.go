package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

const (
	reviewRequiredChecks = "required_status_checks"
	reviewCheckSuccess   = "success"
)

// ReviewOnlyBypass proves the requirements an admin merge would otherwise
// bypass. Unsupported requirements are refusals, never assumed satisfied.
// Classic protection is deliberately unsupported: the effective-rules endpoint
// does not include it, and a partial protection read cannot justify a bypass.
func ReviewOnlyBypass(ctx context.Context, run GH, repo, base, head string) error {
	ctx = ActingOn(ctx, repo)
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || base == "" || head == "" {
		return fmt.Errorf("unknown repository, base or head")
	}
	raw, err := run(ctx, "api", graphqlEndpoint, "-f", `query=query($owner:String!,$name:String!,$ref:String!){repository(owner:$owner,name:$name){ref(qualifiedName:$ref){branchProtectionRule{id}}}}`, "-f", "owner="+parts[0], "-f", "name="+parts[1], "-f", "ref=refs/heads/"+base)
	if err != nil {
		return fmt.Errorf("read classic protection: %w", err)
	}
	var protection struct {
		Data struct {
			Repository *struct {
				Ref *struct {
					Rule json.RawMessage `json:"branchProtectionRule"`
				} `json:"ref"`
			} `json:"repository"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(raw, &protection); err != nil {
		return err
	}
	if len(protection.Errors) != 0 || protection.Data.Repository == nil || protection.Data.Repository.Ref == nil || len(protection.Data.Repository.Ref.Rule) == 0 {
		return fmt.Errorf("classic protection is unreadable")
	}
	if string(protection.Data.Repository.Ref.Rule) != "null" {
		return fmt.Errorf("classic protection cannot be safely waived by the review-only exception")
	}
	rules, err := bypassPages[bypassRule](ctx, run, "repos/"+repo+"/rules/branches/"+url.PathEscape(base))
	if err != nil {
		return fmt.Errorf("read effective rules: %w", err)
	}
	var required []bypassCheck
	for _, rule := range rules {
		switch rule.Type {
		case "deletion", "non_fast_forward": // A squash merge neither deletes nor force-pushes the base.
		case "pull_request":
			if rule.Parameters == nil {
				return fmt.Errorf("pull request requirements are unreadable")
			}
			for key, value := range rule.Parameters {
				switch key {
				case "required_approving_review_count", "dismiss_stale_reviews_on_push", "require_code_owner_review", "require_last_push_approval", "dismissal_restriction", "required_reviewers":
				case "allowed_merge_methods":
					var methods []string
					if json.Unmarshal(value, &methods) != nil || !slices.Contains(methods, "squash") {
						return fmt.Errorf("squash merge is not allowed")
					}
				case "required_review_thread_resolution":
					if string(value) != "false" {
						return fmt.Errorf("unverified requirement: review thread resolution")
					}
				default:
					return fmt.Errorf("unsupported pull request requirement: %s", key)
				}
			}
		case reviewRequiredChecks:
			for key := range rule.Parameters {
				switch key {
				case reviewRequiredChecks, "strict_required_status_checks_policy", "do_not_enforce_on_create":
				default:
					return fmt.Errorf("unsupported status check requirement: %s", key)
				}
			}
			// Admin must not waive an up-to-date requirement. Green checks
			// on a behind head do not prove its merge result is green.
			var strict bool
			if value, ok := rule.Parameters["strict_required_status_checks_policy"]; ok {
				if err := json.Unmarshal(value, &strict); err != nil || string(value) == "null" {
					return fmt.Errorf("up-to-date requirement is unreadable")
				}
			}
			if strict {
				raw, err := run(ctx, "api", "repos/"+repo+"/compare/"+url.PathEscape(base)+"..."+url.PathEscape(head))
				if err != nil {
					return fmt.Errorf("read distance from base: %w", err)
				}
				var distance struct {
					Behind *int `json:"behind_by"`
				}
				if json.Unmarshal(raw, &distance) != nil || distance.Behind == nil || *distance.Behind < 0 {
					return fmt.Errorf("distance from base is unreadable")
				}
				if *distance.Behind != 0 {
					return fmt.Errorf("branch must be up to date: %d commits behind its base", *distance.Behind)
				}
			}
			var checks []bypassCheck
			if err := json.Unmarshal(rule.Parameters[reviewRequiredChecks], &checks); err != nil || checks == nil {
				return fmt.Errorf("required contexts are unreadable")
			}
			for _, c := range checks {
				if c.Context == "" {
					return fmt.Errorf("required context is empty")
				}
			}
			required = append(required, checks...)
		default:
			return fmt.Errorf("unverified non-review requirement: %s", rule.Type)
		}
	}
	if len(required) == 0 {
		return nil
	}
	return bypassCheckResults(ctx, run, repo, head, required)
}

type bypassRule struct {
	Type       string                     `json:"type"`
	Parameters map[string]json.RawMessage `json:"parameters"`
}

type bypassCheck struct {
	Context       string `json:"context"`
	IntegrationID int64  `json:"integration_id"`
}

// Slurped pages must all be arrays; null, empty output and malformed replies
// cannot silently become an empty set of requirements or results.
func bypassPages[T any](ctx context.Context, run GH, path string) ([]T, error) {
	raw, err := run(ctx, "api", "--paginate", "--slurp", path)
	if err != nil {
		return nil, err
	}
	var pages []json.RawMessage
	if err := json.Unmarshal(raw, &pages); err != nil || len(pages) == 0 {
		return nil, fmt.Errorf("unreadable pages for %s", path)
	}
	var all []T
	for _, page := range pages {
		var items []T
		if err := json.Unmarshal(page, &items); err != nil || items == nil {
			return nil, fmt.Errorf("unreadable page for %s", path)
		}
		all = append(all, items...)
	}
	return all, nil
}

func bypassCheckResults(ctx context.Context, run GH, repo, head string, required []bypassCheck) error {
	raw, err := run(ctx, "api", "--paginate", "--slurp", "repos/"+repo+"/commits/"+head+"/check-runs?per_page=100&filter=latest")
	if err != nil {
		return err
	}
	var pages []struct {
		Runs []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			App        struct {
				ID int64 `json:"id"`
			} `json:"app"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(raw, &pages); err != nil || len(pages) == 0 {
		return fmt.Errorf("check runs are unreadable")
	}
	statuses, err := bypassPages[struct {
		ID      int64  `json:"id"`
		Context string `json:"context"`
		State   string `json:"state"`
	}](ctx, run, "repos/"+repo+"/commits/"+head+"/statuses?per_page=100")
	if err != nil {
		return err
	}
	for _, want := range required {
		var runID, statusID int64
		runOK, statusOK := false, false
		for _, page := range pages {
			if page.Runs == nil {
				return fmt.Errorf("check runs are unreadable")
			}
			for _, check := range page.Runs {
				if check.Name == want.Context && (want.IntegrationID <= 0 || check.App.ID == want.IntegrationID) && check.ID > runID {
					runID = check.ID
					runOK = check.Status == "completed" && slices.Contains([]string{reviewCheckSuccess, "neutral", "skipped"}, check.Conclusion)
				}
			}
		}
		for _, status := range statuses {
			if status.Context == want.Context && status.ID > statusID {
				statusID, statusOK = status.ID, status.State == reviewCheckSuccess
			}
		}
		// When both a check and a status have the name, GitHub requires both.
		// A source-bound requirement needs a check from that App; a legacy status
		// cannot prove its integration identity.
		if (runID == 0 && statusID == 0) || (want.IntegrationID > 0 && runID == 0) || (runID != 0 && !runOK) || (statusID != 0 && !statusOK) {
			return fmt.Errorf("required context %q is missing or unsuccessful on %s (integration %d)", want.Context, head, want.IntegrationID)
		}
	}
	return nil
}
