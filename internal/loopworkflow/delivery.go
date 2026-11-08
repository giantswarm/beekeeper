package loopworkflow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const deliveryClosedIssueFile = "issue-2-closed"

// deliveryGitHub supplies only the external observations peculiar to the WIP
// scenario; parsing, admission, planning and merging stay in the candidate.
func deliveryGitHub(args []string) (int, bool) {
	if os.Getenv("FIXTURE_CASE") != deliveryScenario {
		return 0, false
	}
	emit := func(v any) (int, bool) {
		if err := json.NewEncoder(os.Stdout).Encode(v); err != nil {
			panic(err)
		}
		return 0, true
	}
	query := strings.Join(args, " ")
	switch {
	case len(args) == 9 && slices.Equal(args[:7], []string{"issue", "close", "2", "--repo", "fixture/project", "--reason", "completed"}) && args[7] == "--comment":
		if err := os.WriteFile(filepath.Join(os.Getenv("FIXTURE_DIR"), deliveryClosedIssueFile), []byte("closed\n"), 0600); err != nil { //nolint:gosec // isolated scratch issue observation
			panic(err)
		}
		return 0, true
	case len(args) == 3 && args[0] == ghAPI && args[1] == "-i" && slices.Contains([]string{
		"repos/fixture/project/issues/3/comments?per_page=100&page=1",
		"repos/fixture/project/issues/4/comments?per_page=100&page=1",
		"repos/fixture/project/issues/5/comments?per_page=100&page=1",
	}, args[2]):
		fmt.Print("HTTP/2.0 200 OK\r\nContent-Type: application/json\r\n\r\n[]")
		return 0, true
	case slices.Contains(args, ghGraphQL) && strings.Contains(query, "issue(number:$number){parent{url}}"):
		return emit(map[string]any{dataField: map[string]any{repositoryField: map[string]any{"issue": map[string]any{"parent": nil}}}})
	case slices.Contains(args, ghGraphQL) && strings.Contains(query, "items(first:"):
		var nodes []any
		for n := 2; n <= 5; n++ {
			if n == 2 {
				if _, err := os.Stat(filepath.Join(os.Getenv("FIXTURE_DIR"), deliveryClosedIssueFile)); err == nil {
					continue
				} //nolint:gosec // isolated scratch issue observation
			}
			nodes = append(nodes, map[string]any{"id": fmt.Sprintf("I%d", n), statusField: map[string]any{"name": "Ready"}, "content": map[string]any{
				"number": n, "url": fmt.Sprintf("https://github.com/fixture/project/issues/%d", n), "title": fmt.Sprintf("fixture task %d", n), "state": "OPEN",
				"createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z", repositoryField: map[string]any{"nameWithOwner": "fixture/project"},
				"blockedBy": map[string]any{"totalCount": 0, "nodes": []any{}}, "subIssuesSummary": map[string]any{"total": 0, "completed": 0},
			}})
		}
		return emit(map[string]any{dataField: map[string]any{"repositoryOwner": map[string]any{"projectV2": map[string]any{"items": map[string]any{"pageInfo": map[string]any{"hasNextPage": false}, "nodes": nodes}}}}})
	case slices.Contains(args, ghGraphQL) && strings.Contains(query, "issueOrPullRequest(number:"):
		// Ref state batches use r0, r1, ... aliases. The only PR is 1;
		// issue 2 closes when the merge closes its linked issue.
		data := map[string]any{}
		for i := 0; i < 8; i++ {
			alias := fmt.Sprintf("r%d", i)
			pos := strings.Index(query, alias+":")
			if pos < 0 {
				continue
			}
			part := query[pos:]
			kind, status := "Issue", "OPEN"
			if strings.Contains(strings.SplitN(part, "}", 2)[0], "number:2)") {
				if _, err := os.Stat(filepath.Join(os.Getenv("FIXTURE_DIR"), deliveryClosedIssueFile)); err == nil {
					status = "CLOSED"
				} //nolint:gosec // isolated scratch issue observation
			}
			if strings.Contains(strings.SplitN(part, "}", 2)[0], "number:1)") {
				kind = "PullRequest"
				raw, err := os.ReadFile(filepath.Join(os.Getenv("FIXTURE_DIR"), pullJSONFile)) //nolint:gosec // isolated scratch fixture, same boundary as serveGitHub
				if err != nil {
					panic(err)
				}
				var p map[string]any
				if err := json.Unmarshal(raw, &p); err != nil {
					panic(err)
				}
				status, _ = p[stateField].(string)
			}
			data[alias] = map[string]any{"issueOrPullRequest": map[string]any{"__typename": kind, "state": status}}
		}
		return emit(map[string]any{dataField: data})
	case len(args) == 4 && args[0] == ghAPI && args[1] == "-H" && strings.Contains(args[3], "/contents/.github/workflows/zz_generated.auto_release.yaml?"):
		fmt.Print("on:\n  push:\n    branches: [main]\n")
		return 0, true
	case len(args) > 1 && args[0] == ghAPI && strings.Contains(args[1], "/tags?per_page=1"):
		if _, err := os.Stat(filepath.Join(os.Getenv("FIXTURE_DIR"), "released")); err == nil { //nolint:gosec // isolated scratch fixture release marker
			fmt.Println("v1.0.1-rc.1")
		}
		return 0, true
	case len(args) > 1 && args[0] == ghAPI && strings.Contains(args[1], "/compare/"):
		fmt.Println("ahead")
		return 0, true
	default:
		return 0, false
	}
}
