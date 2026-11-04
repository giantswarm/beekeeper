package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMentions(t *testing.T) {
	for text, want := range map[string]string{
		"thanks @marians, fixed":           "marians",
		"(@QuentinBisson) see above":       "QuentinBisson",
		"@fiunchinho":                      "fiunchinho",
		"git@github.com:giantswarm/x":      "",
		"go install x@latest":              "",
		"npm i @types/node":                "types/node",
		"please @example/review-team":      "example/review-team",
		"(@example/team_name), thanks":     "example/team_name",
		"`@example/review-team`":           "",
		"mail me at @example.com":          "",
		"the `@marians` handle, an @-ping": "",
		"gh api -F body=@body.md":          "",
		"no mention here":                  "",
	} {
		if got := strings.Join(mentions(text), ","); got != want {
			t.Errorf("mentions(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestMentionRefusal(t *testing.T) {
	const ping, bodyKey = "cc @piontec", "body"
	dir := t.TempDir()
	body := filepath.Join(dir, "body.md")
	if err := os.WriteFile(body, []byte("Done.\n\n@QuentinBisson can you check?\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clean := filepath.Join(dir, "clean.md")
	if err := os.WriteFile(clean, []byte("Done, QuentinBisson's PR is merged.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bash := func(cmd string) map[string]any { return map[string]any{commandKey: cmd} }
	for _, c := range []struct {
		tool   string
		input  map[string]any
		refuse bool
	}{
		{bashTool, bash(`gh pr comment 12 --repo giantswarm/x --body "thanks @marians"`), true},
		{bashTool, bash(`gh issue comment 3 --repo example/project --body "please @example/review-team"`), true},
		{bashTool, bash(`gh issue create --title t --repo example/project --body "please @example/review-team"`), true},
		{bashTool, bash(`gh pr comment 3 --repo example/project --body "please @example/review-team"`), true},
		{bashTool, bash(`gh pr create --title t --repo example/project --body "please @example/review-team"`), true},
		{bashTool, bash(`gh issue comment 3 --repo giantswarm/x --body-file ` + body), true},
		{bashTool, bash(`gh api repos/giantswarm/x/issues/3/comments -F body=@` + body), true},
		{bashTool, bash(`gh api -X POST repos/giantswarm/x/issues -f title=t -f body="cc @marians"`), true},
		{bashTool, bash(`gh api repos/giantswarm/x/issues/3/comments -f body="@marians see" --jq '.id | @text'`), true},
		{bashTool, bash(`gh api repos/giantswarm/x/issues/3/comments -f body=done --jq '[.id, .html_url] | @tsv'`), false},
		{bashTool, bash(`gh issue create --repo giantswarm/x --title t --body-file ` + clean), false},
		{bashTool, bash(`echo "@marians" && gh pr view 12`), false},
		{bashTool, bash(`gh api repos/giantswarm/x/issues --jq '.[] | [.number, .title] | @tsv'`), false},
		{bashTool, bash(`gh api repos/giantswarm/x/pulls -q '.[] | [.number] | @csv'`), false},
		{bashTool, bash(`gh api repos/giantswarm/x/issues/3/comments --jq='.[].body | @json, @sh, @base64, @uri, @html'`), false},
		{bashTool, bash(`gh api -X GET repos/giantswarm/x/issues -f state=open --jq '.[] | @tsv'`), false},
		{bashTool, bash(`git commit -m "fix: thanks @marians"`), false},
		{"mcp__claude_ai_GitHub_MCP__add_issue_comment", map[string]any{bodyKey: ping}, true},
		{"mcp__claude_ai_GitHub_MCP__issue_read", map[string]any{bodyKey: ping}, false},
		{"mcp__claude_ai_Slack__slack_send_message", map[string]any{bodyKey: ping}, false},
	} {
		if got := mentionRefusal(c.tool, c.input, dir) != ""; got != c.refuse {
			t.Errorf("%s %v: refused %v, want %v", c.tool, c.input, got, c.refuse)
		}
	}
}

func TestTeamMentionRefusalBodyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "body.md"), []byte("Please @example/review-team take a look."), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"issue", "pr"} {
		t.Run(kind, func(t *testing.T) {
			reason := mentionRefusal(bashTool, map[string]any{commandKey: "gh " + kind + " comment 3 --body-file body.md"}, dir)
			for _, want := range []string{"Refused:", "example/review-team", "ask the supervisor"} {
				if !strings.Contains(reason, want) {
					t.Errorf("refusal %q does not contain %q", reason, want)
				}
			}
		})
	}
}
