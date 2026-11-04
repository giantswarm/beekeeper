package guard

import (
	"encoding/json"
	"reflect"
	"testing"
)

const (
	prListClass = "gh pr list"
	prViewClass = "gh pr view"
)

func TestGraphQLCalls(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    []GitHubCall
	}{
		{`gh project item-list 1 --owner private --query secret`, []GitHubCall{{"gh project item-list", true}}},
		{`TOKEN=private env gh api graphql --paginate -f query=private && gh pr checks 1`, []GitHubCall{{"gh api graphql", true}, {"gh pr checks", false}}},
		{`/usr/bin/gh issue list; gh issue view 2; gh pr view 2; gh pr list; gh release list; beekeeper board next`, []GitHubCall{{"gh issue list", false}, {"gh issue view", false}, {prViewClass, false}, {prListClass, false}, {"gh release list", false}, {"beekeeper board", false}}},
		{`echo gh project item-list; gh api repos/private; gh release view; gh issue create; # gh pr list`, nil},
		{`gh -R private/repo pr view 2`, []GitHubCall{{prViewClass, false}}},
		{`gh project item-list --help`, nil},
		{`gh project item-add 1 --url private`, []GitHubCall{{"gh project item-add", false}}},
	} {
		t.Run(tc.command, func(t *testing.T) {
			if got := graphQLCalls(tc.command); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestGitHubCallsFormats(t *testing.T) {
	for _, tc := range []struct{ format, payload string }{
		{"claude", `{"session_id":"s1","tool_name":"Bash","tool_input":{"command":"gh pr list"}}`},
		{secretLinkCodex, `{"session_id":"s1","tool_name":"exec_command","tool_input":{"cmd":"gh pr list"}}`},
		{"agy", `{"conversationId":"s1","toolCall":{"name":"run_command","args":{"CommandLine":"gh pr list"}}}`},
	} {
		t.Run(tc.format, func(t *testing.T) {
			session, calls := GitHubCalls([]byte(tc.payload), tc.format)
			if session != "s1" || len(calls) != 1 || calls[0].Class != prListClass {
				t.Fatalf("%s %+v", session, calls)
			}
		})
	}
	raw, _ := json.Marshal(map[string]any{sessionKey: "s1", executionToolName: "Write", executionToolInput: map[string]string{commandKey: prListClass}})
	if _, calls := GitHubCalls(raw, "claude"); len(calls) != 0 {
		t.Fatal(calls)
	}
}
