package loopworkflow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// deliveryGitHub supplies only the external observations peculiar to the WIP
// scenario; parsing, admission, planning and merging stay in the candidate.
func deliveryGitHub(args []string) (int, bool) {
	if os.Getenv("FIXTURE_CASE") != "delivery" {
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
	case slices.Contains(args, "graphql") && strings.Contains(query, "items(first:"):
		var nodes []any
		for n := 2; n <= 5; n++ {
			nodes = append(nodes, map[string]any{"id": fmt.Sprintf("I%d", n), "status": map[string]any{"name": "Ready"}, "content": map[string]any{
				"number": n, "url": fmt.Sprintf("https://github.com/fixture/project/issues/%d", n), "title": fmt.Sprintf("fixture task %d", n), "state": "OPEN",
				"createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z", "repository": map[string]any{"nameWithOwner": "fixture/project"},
				"blockedBy": map[string]any{"totalCount": 0, "nodes": []any{}}, "subIssuesSummary": map[string]any{"total": 0, "completed": 0},
			}})
		}
		return emit(map[string]any{"data": map[string]any{"repositoryOwner": map[string]any{"projectV2": map[string]any{"items": map[string]any{"pageInfo": map[string]any{"hasNextPage": false}, "nodes": nodes}}}}})
	case slices.Contains(args, "graphql") && strings.Contains(query, "issueOrPullRequest(number:"):
		// Ref state batches use r0, r1, ... aliases. The only PR is 1;
		// all four board issues remain open throughout this scenario.
		data := map[string]any{}
		for i := 0; i < 8; i++ {
			alias := fmt.Sprintf("r%d", i)
			pos := strings.Index(query, alias+":")
			if pos < 0 {
				continue
			}
			part := query[pos:]
			kind, status := "Issue", "OPEN"
			if strings.Contains(strings.SplitN(part, "}", 2)[0], "number:1)") {
				kind = "PullRequest"
				raw, err := os.ReadFile(filepath.Join(os.Getenv("FIXTURE_DIR"), pullJSONFile))
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
		return emit(map[string]any{"data": data})
	case len(args) == 4 && args[0] == "api" && args[1] == "-H" && strings.Contains(args[3], "/contents/.github/workflows/zz_generated.auto_release.yaml?"):
		fmt.Print("on:\n  push:\n    branches: [main]\n")
		return 0, true
	case len(args) > 1 && args[0] == "api" && strings.Contains(args[1], "/tags?per_page=1"):
		if _, err := os.Stat(filepath.Join(os.Getenv("FIXTURE_DIR"), "released")); err == nil {
			fmt.Println("v1.0.1-rc.1")
		}
		return 0, true
	case len(args) > 1 && args[0] == "api" && strings.Contains(args[1], "/compare/"):
		fmt.Println("ahead")
		return 0, true
	default:
		return 0, false
	}
}
