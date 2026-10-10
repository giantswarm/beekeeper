package guard

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBoardReadRefusal(t *testing.T) {
	left := func() string { return "GraphQL 4200 of 5000, read 1m ago" }
	for name, tc := range map[string]struct {
		cmd     string
		refused bool
	}{
		"the org board item-list":            {"gh project item-list 273 --owner giantswarm --limit 3000 --format json", true},
		"item-list by path":                  {"/usr/bin/gh project item-list 273 --owner giantswarm", true},
		"item-list after a cd":               {"cd /tmp && gh project item-list 1 --owner @me", true},
		"item-list piped into jq":            {"gh project item-list 273 --owner giantswarm -L 3000 --format json | jq '.items[]'", true},
		"item-list narrowed by a query":      {`gh project item-list 273 --owner giantswarm --query "repo:giantswarm/beekeeper -status:Done"`, false},
		"item-list narrowed, = form":         {`gh project item-list 273 --owner giantswarm --query="assignee:teemow"`, false},
		"item-add":                           {"gh project item-add 273 --owner giantswarm --url https://github.com/giantswarm/beekeeper/issues/487", false},
		"item-edit by item id":               {"gh project item-edit --id PVTI_x --project-id PVT_y --field-id PVTF_z --single-select-option-id abc", false},
		"project view":                       {"gh project view 273 --owner giantswarm", false},
		"field-list":                         {"gh project field-list 273 --owner giantswarm", false},
		"board move":                         {"beekeeper board move giantswarm/beekeeper#487 Done", false},
		"a mention in a comment body":        {"gh issue comment 1 --body \"the hook refuses `gh project item-list 273`\"", false},
		"a grep for it":                      {"grep -rn 'gh project item-list' docs/", false},
		"graphql paging projectV2 items":     {`gh api graphql --paginate -f query='query($endCursor: String) { organization(login: "giantswarm") { projectV2(number: 273) { items(first: 100, after: $endCursor) { nodes { id } pageInfo { hasNextPage endCursor } } } } }'`, true},
		"graphql items without query, lines": {"gh api graphql -f query='\nquery {\n  organization(login: \"giantswarm\") {\n    projectV2(number: 273) {\n      items(first: 100) { nodes { id } }\n    }\n  }\n}'", true},
		"graphql items narrowed by a query":  {`gh api graphql -f query='{ organization(login: "giantswarm") { projectV2(number: 273) { items(first: 20, query: "repo:giantswarm/beekeeper") { nodes { id } } } } }'`, false},
		"graphql one issue's projectItems":   {`gh api graphql -f query='{ repository(owner: "giantswarm", name: "beekeeper") { issue(number: 487) { projectItems(first: 10) { nodes { id project { number } } } } } }'`, false},
		"graphql over another items":         {`gh api graphql -f query='{ viewer { lists(first: 5) { nodes { items(first: 100) { nodes { __typename } } } } } }'`, false},
		"a rest call":                        {"gh api repos/giantswarm/beekeeper/issues/487", false},
	} {
		t.Run(name, func(t *testing.T) {
			r := boardReadRefusal(tc.cmd, left)
			if (r != "") != tc.refused {
				t.Fatalf("boardReadRefusal(%q) = %q, want refused %v", tc.cmd, r, tc.refused)
			}
			if !tc.refused {
				return
			}
			for _, want := range []string{"gh project item-add", "--id <item-id>", "beekeeper board move", "GraphQL 4200 of 5000", "estimated"} {
				if !strings.Contains(r, want) {
					t.Errorf("the refusal does not name %q: %q", want, r)
				}
			}
		})
	}
	if r := (Hook{}).boardReadRefusal("gh project item-list 273 --owner giantswarm"); !strings.Contains(r, "GraphQL left: unknown") {
		t.Errorf("no reading, the refusal does not say the budget is unknown: %q", r)
	}
}

func TestDecideRefusesBoardRead(t *testing.T) {
	read := false
	h := Hook{Self: hookSelf, Shell: hookShell, GraphQL: func() string { read = true; return "GraphQL 1 of 5000" }}
	decide := func(cmd string) hookOutput {
		raw, _ := json.Marshal(event{ToolName: bashTool, ToolInput: map[string]any{commandKey: cmd}, CWD: t.TempDir()})
		var o map[string]hookOutput
		if out := h.Decide(raw); out != nil {
			if err := json.Unmarshal(out, &o); err != nil {
				t.Fatal(err)
			}
		}
		return o["hookSpecificOutput"]
	}
	if d := decide("gh project item-add 273 --owner giantswarm --url https://github.com/giantswarm/beekeeper/issues/487"); d.PermissionDecision == decisionDeny || read {
		t.Errorf("item-add refused (%q) or the budget read for it (%v)", d.Reason, read)
	}
	if d := decide("gh project item-list 273 --owner giantswarm --limit 3000"); d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "GraphQL 1 of 5000") {
		t.Errorf("item-list not refused with the budget: %+v", d)
	}
}
