//nolint:goconst // Keep failure scenarios and wire-protocol fixtures readable.
package github

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestMergeResultGreenRequiresExactCombinationAndChecks(t *testing.T) {
	baseSHA, headSHA := "merge-base-sha", "merge-head-sha"
	for _, mode := range []string{
		"merge-green", "merge-old-base", "merge-new-head", "merge-wrong-parent",
		"merge-base-advanced", "merge-head-checks-only", "merge-check-pending",
		"merge-check-failed", "merge-wrong-app", "merge-unprotected",
		"merge-null-checks", "merge-classic-missing", "merge-legacy-red",
	} {
		t.Run(mode, func(t *testing.T) {
			run := func(_ context.Context, args ...string) ([]byte, error) {
				path := args[len(args)-1]
				var out string
				switch {
				case strings.HasSuffix(path, "/pulls/8"):
					base, head := baseSHA, headSHA
					if mode == "merge-old-base" {
						base = "stale-base-sha"
					}
					if mode == "merge-new-head" {
						head = "newer-head-sha"
					}
					out = fmt.Sprintf(`{"merge_commit_sha":"merge","base":{"sha":%q,"ref":"main"},"head":{"sha":%q}}`, base, head)
				case strings.HasSuffix(path, "/git/commits/merge"):
					parent := baseSHA
					if mode == "merge-wrong-parent" {
						parent = "stale-parent-sha"
					}
					out = fmt.Sprintf(`{"sha":"merge","parents":[{"sha":%q},{"sha":"merge-head-sha"}]}`, parent)
				case strings.HasSuffix(path, "/rules/branches/main"):
					out = `[[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"test","integration_id":42}]}}]]`
					if mode == "merge-unprotected" {
						out = `[ [] ]`
					}
				case strings.HasSuffix(path, "/branches/main"):
					out = `{"commit":{"sha":"merge-base-sha"}}`
					if mode == "merge-base-advanced" {
						out = `{"commit":{"sha":"new-base"}}`
					}
					if mode == "merge-classic-missing" {
						out = `{"commit":{"sha":"merge-base-sha"},"protection":{"required_status_checks":{"contexts":["classic"]}}}`
					}
				case strings.Contains(path, "/commits/merge/check-runs?"):
					out = `[{"check_runs":[{"id":1,"name":"test","status":"completed","conclusion":"success","app":{"id":42}}]}]`
					switch mode {
					case "merge-check-pending":
						out = `[{"check_runs":[{"id":1,"name":"test","status":"in_progress","conclusion":"success","app":{"id":42}}]}]`
					case "merge-check-failed":
						out = `[{"check_runs":[{"id":1,"name":"test","status":"completed","conclusion":"failure","app":{"id":42}}]}]`
					case "merge-wrong-app":
						out = `[{"check_runs":[{"id":1,"name":"test","status":"completed","conclusion":"success","app":{"id":7}}]}]`
					case "merge-head-checks-only":
						out = `[{"check_runs": []}]`
					case "merge-null-checks":
						out = nullResponseFixture
					}
				case strings.Contains(path, "/commits/merge/statuses?"):
					out = `[  []  ]`
					if mode == "merge-legacy-red" {
						out = `[[{"id":2,"context":"test","state":"failure"}]]`
					}
				default:
					t.Fatalf("unexpected request: %v", args)
				}
				return []byte(out), nil
			}
			green, err := MergeResultGreen(t.Context(), run, headsRepo, mainBranch, baseSHA, headSHA, 8)
			if green != (mode == "merge-green") {
				t.Fatalf("green=%v err=%v", green, err)
			}
		})
	}
}
