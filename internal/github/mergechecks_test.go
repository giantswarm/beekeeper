//nolint:goconst // Keep failure scenarios and wire-protocol fixtures readable.
package github

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestMergeResultGreenRequiresExactCombinationAndChecks(t *testing.T) {
	for _, mode := range []string{"green", "old base", "new head", "wrong parent", "base advanced", "head checks only", "pending", "failed", "wrong app", "no requirements", "unreadable", "classic missing", "legacy red"} {
		t.Run(mode, func(t *testing.T) {
			run := func(_ context.Context, args ...string) ([]byte, error) {
				path := args[len(args)-1]
				var out string
				switch {
				case strings.HasSuffix(path, "/pulls/8"):
					base, head := "base", "head"
					if mode == "old base" {
						base = "old"
					}
					if mode == "new head" {
						head = "new"
					}
					out = fmt.Sprintf(`{"merge_commit_sha":"merge","base":{"sha":%q,"ref":"main"},"head":{"sha":%q}}`, base, head)
				case strings.HasSuffix(path, "/git/commits/merge"):
					parent := "base"
					if mode == "wrong parent" {
						parent = "old"
					}
					out = fmt.Sprintf(`{"sha":"merge","parents":[{"sha":%q},{"sha":"head"}]}`, parent)
				case strings.HasSuffix(path, "/rules/branches/main"):
					out = `[[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"test","integration_id":42}]}}]]`
					if mode == "no requirements" {
						out = `[[]]`
					}
				case strings.HasSuffix(path, "/branches/main"):
					out = `{"commit":{"sha":"base"}}`
					if mode == "base advanced" {
						out = `{"commit":{"sha":"new-base"}}`
					}
					if mode == "classic missing" {
						out = `{"commit":{"sha":"base"},"protection":{"required_status_checks":{"contexts":["classic"]}}}`
					}
				case strings.Contains(path, "/commits/merge/check-runs?"):
					status, conclusion, app := "completed", "success", 42
					if mode == "pending" {
						status = "in_progress"
					}
					if mode == "failed" {
						conclusion = "failure"
					}
					if mode == "wrong app" {
						app = 7
					}
					out = fmt.Sprintf(`[{"check_runs":[{"id":1,"name":"test","status":%q,"conclusion":%q,"app":{"id":%d}}]}]`, status, conclusion, app)
					if mode == "head checks only" {
						out = `[{"check_runs":[]}]`
					}
					if mode == "unreadable" {
						out = `null`
					}
				case strings.Contains(path, "/commits/merge/statuses?"):
					out = `[[]]`
					if mode == "legacy red" {
						out = `[[{"id":2,"context":"test","state":"failure"}]]`
					}
				default:
					t.Fatalf("unexpected request: %v", args)
				}
				return []byte(out), nil
			}
			green, err := MergeResultGreen(t.Context(), run, "o/r", "main", "base", "head", 8)
			if green != (mode == "green") {
				t.Fatalf("green=%v err=%v", green, err)
			}
		})
	}
}
