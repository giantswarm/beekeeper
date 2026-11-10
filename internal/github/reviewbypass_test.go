package github

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const reviewClassicJSON = `{"data":{"repository":{"ref":{"branchProtectionRule":null}}}}`
const reviewRulesJSON = `[[{"type":"pull_request","parameters":{"required_approving_review_count":1,"required_review_thread_resolution":false}},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"security-scan","integration_id":42}]}}]]`
const reviewRunsJSON = `[{"check_runs":[{"id":3,"name":"security-scan","status":"completed","conclusion":"success","app":{"id":42}}]}]`

func TestReviewOnlyBypassRequirements(t *testing.T) {
	for _, tc := range []struct {
		name, classic, rules, runs, statuses, failedPath, distance string
		wantError                                                  bool
	}{
		{name: "required check passes"},
		{name: "dismissal restriction", rules: strings.Replace(reviewRulesJSON, `"required_approving_review_count":1`, `"required_approving_review_count":1,"dismissal_restriction":{"users":[],"teams":[]}`, 1)},
		{name: "strict current", rules: strings.Replace(reviewRulesJSON, `"required_status_checks":[`, `"strict_required_status_checks_policy":true,"required_status_checks":[`, 1), distance: `{"behind_by":0}`},
		{name: "strict behind", rules: strings.Replace(reviewRulesJSON, `"required_status_checks":[`, `"strict_required_status_checks_policy":true,"required_status_checks":[`, 1), distance: `{"behind_by":2}`, wantError: true},
		{name: "strict unreadable distance", rules: strings.Replace(reviewRulesJSON, `"required_status_checks":[`, `"strict_required_status_checks_policy":true,"required_status_checks":[`, 1), distance: `{}`, wantError: true},
		{name: "malformed strict", rules: strings.Replace(reviewRulesJSON, `"required_status_checks":[`, `"strict_required_status_checks_policy":null,"required_status_checks":[`, 1), wantError: true},
		{name: "missing required check beside green lint", runs: `[{"check_runs":[{"id":1,"name":"lint","status":"completed","conclusion":"success"}]}]`, wantError: true},
		{name: "wrong integration", runs: strings.ReplaceAll(reviewRunsJSON, `"id":42`, `"id":99`), wantError: true},
		{name: "pending", runs: strings.ReplaceAll(reviewRunsJSON, `"completed"`, `"in_progress"`), wantError: true},
		{name: "failed", runs: strings.ReplaceAll(reviewRunsJSON, `"success"`, `"failure"`), wantError: true},
		{name: "neutral accepted", runs: strings.ReplaceAll(reviewRunsJSON, `"success"`, `"neutral"`)},
		{name: "skipped accepted", runs: strings.ReplaceAll(reviewRunsJSON, `"success"`, `"skipped"`)},
		{name: "legacy status cannot impersonate app", runs: `[{"check_runs":[]}]`, statuses: `[[{"id":1,"context":"security-scan","state":"success"}]]`, wantError: true},
		{name: "both status and check must pass", statuses: `[[{"id":1,"context":"security-scan","state":"failure"}]]`, wantError: true},
		{name: "latest status wins", statuses: `[[{"id":2,"context":"security-scan","state":"failure"},{"id":1,"context":"security-scan","state":"success"}]]`, wantError: true},
		{name: "latest run wins across pages", runs: `[{"check_runs":[{"id":3,"name":"security-scan","status":"completed","conclusion":"success","app":{"id":42}}]},{"check_runs":[{"id":4,"name":"security-scan","status":"completed","conclusion":"failure","app":{"id":42}}]}]`, wantError: true},
		{name: "unbound legacy status", rules: strings.ReplaceAll(reviewRulesJSON, `,"integration_id":42`, ``), runs: `[{"check_runs":[]}]`, statuses: `[[{"id":1,"context":"security-scan","state":"success"}]]`},
		{name: "no requirements", rules: `[[]]`},
		{name: "squash forbidden", rules: `[[{"type":"pull_request","parameters":{"allowed_merge_methods":["merge"]}}]]`, wantError: true},
		{name: "unknown pull requirement", rules: `[[{"type":"pull_request","parameters":{"future":true}}]]`, wantError: true},
		{name: "unknown check requirement", rules: `[[{"type":"required_status_checks","parameters":{"future":true,"required_status_checks":[]}}]]`, wantError: true},
		{name: "missing rules", rules: `[]`, wantError: true},
		{name: "null rules", rules: `[null]`, wantError: true},
		{name: "malformed rules", rules: `{}`, wantError: true},
		{name: "missing contexts", rules: `[[{"type":"required_status_checks","parameters":{}}]]`, wantError: true},
		{name: "missing check runs", runs: `[{}]`, wantError: true},
		{name: "missing statuses", statuses: `[null]`, wantError: true},
		{name: "classic protection", classic: `{"data":{"repository":{"ref":{"branchProtectionRule":{"id":"rule"}}}}}`, wantError: true},
		{name: "unreadable classic", classic: `{"data":{"repository":{"ref":null}}}`, wantError: true},
		{name: "partial GraphQL", classic: `{"data":{"repository":{"ref":{"branchProtectionRule":null}}},"errors":[{"message":"denied"}]}`, wantError: true},
		{name: "unresolved threads", rules: `[[{"type":"pull_request","parameters":{"required_review_thread_resolution":true}}]]`, wantError: true},
		{name: "deployment", rules: `[[{"type":"required_deployments"}]]`, wantError: true},
		{name: "signatures", rules: `[[{"type":"required_signatures"}]]`, wantError: true},
		{name: "merge queue", rules: `[[{"type":"merge_queue"}]]`, wantError: true},
		{name: "unknown rule", rules: `[[{"type":"future_rule"}]]`, wantError: true},
		{name: "unreadable rules", failedPath: "rules/branches", wantError: true},
		{name: "unreadable check results", failedPath: "check-runs", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(_ context.Context, args ...string) ([]byte, error) {
				call := strings.Join(args, " ")
				if tc.failedPath != "" && strings.Contains(call, tc.failedPath) {
					return nil, errors.New("HTTP 403")
				}
				answer := ""
				switch {
				case args[1] == graphqlEndpoint:
					answer = tc.classic
					if answer == "" {
						answer = reviewClassicJSON
					}
				case strings.Contains(call, "rules/branches/release%2F1.x"):
					answer = tc.rules
					if answer == "" {
						answer = reviewRulesJSON
					}
				case strings.Contains(call, "/compare/"):
					answer = tc.distance
				case strings.Contains(call, "commits/exact-head/check-runs"):
					answer = tc.runs
					if answer == "" {
						answer = reviewRunsJSON
					}
				case strings.Contains(call, "commits/exact-head/statuses"):
					answer = tc.statuses
					if answer == "" {
						answer = `[[]]`
					}
				default:
					t.Fatalf("unexpected request: %v", args)
				}
				return []byte(answer), nil
			}
			err := ReviewOnlyBypass(context.Background(), run, "o/r", "release/1.x", "exact-head")
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, want error %v", err, tc.wantError)
			}
		})
	}
}
