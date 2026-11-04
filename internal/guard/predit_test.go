package guard

import (
	"strings"
	"testing"
)

func TestPREditRESTGuidance(t *testing.T) {
	h := Hook{}
	for _, cmd := range []string{
		`gh pr edit 12 --title 'New title'`,
		`gh pr edit 12 --body-file body.md`,
		`gh pr --repo o/r edit 12 --add-reviewer org/team`,
		`env GH_REPO=o/r gh pr edit 12 --body 'New body'`,
		`command /usr/bin/gh pr edit 12 -t title`,
		`echo ready && gh pr edit 12 --title title`,
		`bash -c 'gh pr edit 12 --title title'`,
	} {
		t.Run(cmd, func(t *testing.T) {
			got := h.ghPREditRefusal(cmd, t.TempDir())
			for _, want := range []string{"gh api -X PATCH", "-f title=", "-f body=", "-F body=@", "Members read", "beekeeper note add"} {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in %q", want, got)
				}
			}
		})
	}
	for _, cmd := range []string{
		`gh api -X PATCH repos/o/r/pulls/12 -f title='New title' -F body=@body.md`,
		`gh pr view 12`,
		`gh issue edit 12 --title title`,
		`echo 'gh pr edit 12 --title title'`,
		`gh pr comment 12 --body 'please use gh pr edit'`,
	} {
		if got := h.ghPREditRefusal(cmd, t.TempDir()); got != "" {
			t.Errorf("%s: %s", cmd, got)
		}
	}
}

func TestPREditHookClaudeAndAgy(t *testing.T) {
	h := Hook{Self: testBeekeeperExe, Shell: testShell}
	dir := t.TempDir()
	for _, cmd := range []string{`gh pr edit 12 --title title`, `gh pr edit 12 --body "Updated body"`, `gh pr edit 12 --add-reviewer org/team`} {
		if d := decide(t, h, dir, cmd, nil); d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "gh api -X PATCH") {
			t.Fatalf("claude %s: %+v", cmd, d)
		}
		if d := decideAgyCmd(t, h, dir, cmd); d.Decision != decisionDeny || !strings.Contains(d.Reason, "gh api -X PATCH") {
			t.Fatalf("agy %s: %+v", cmd, d)
		}
	}
}

func TestPREditRESTAllowedByHooks(t *testing.T) {
	h := Hook{Self: testBeekeeperExe, Shell: testShell}
	dir := t.TempDir()
	for _, cmd := range []string{
		`gh api -X PATCH repos/o/r/pulls/12 -f title='Updated title' -f body='Updated body'`,
		`gh api -X PATCH repos/o/r/pulls/12 -F body=@body.md`,
	} {
		if d := decide(t, h, dir, cmd, nil); d != nil && d.PermissionDecision == decisionDeny {
			t.Fatalf("claude %s: %+v", cmd, d)
		}
		if d := decideAgyCmd(t, h, dir, cmd); d.Decision == decisionDeny {
			t.Fatalf("agy %s: %+v", cmd, d)
		}
	}
}
