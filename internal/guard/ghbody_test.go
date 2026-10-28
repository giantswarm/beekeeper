package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGHBodyFullURLCheckClaudeAndAgy(t *testing.T) {
	dir := t.TempDir()
	badFile := filepath.Join(dir, "bad.md")
	if err := os.WriteFile(badFile, []byte("Closes #11 and x/y#12\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	goodFile := filepath.Join(dir, "good.md")
	if err := os.WriteFile(goodFile, []byte("Closes https://github.com/o/r/issues/11 (o/r#11)\n\n```sh\ngh pr view #99\n```\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := Hook{Self: testBeekeeperExe, Shell: testShell}
	for _, c := range []struct {
		name, cmd, want string
	}{
		{
			"gh pr create bare #n",
			`gh pr create --repo o/r --title "feat: x" --body "Closes #7"`,
			"#7 without its full URL (https://github.com/o/r/issues/7)",
		},
		{
			"gh pr edit owner/repo#n",
			`gh pr edit 12 --repo o/r --body "Part of x/y#8"`,
			"x/y#8 without its full URL (https://github.com/x/y/issues/8)",
		},
		{
			"gh issue create bare #n",
			`gh issue create -R o/r --title "bug" -b "See #9"`,
			"#9 without its full URL (https://github.com/o/r/issues/9)",
		},
		{
			"gh issue comment bare #n with positional target",
			`gh issue comment o/r#5 --body "Duplicate of #10"`,
			"#10 without its full URL (https://github.com/o/r/issues/10)",
		},
		{
			"gh pr create --body-file",
			`gh pr create --repo o/r --title "feat: x" --body-file ` + badFile,
			"#11 without its full URL (https://github.com/o/r/issues/11)",
		},
		{
			"gh issue comment -F=",
			`gh issue comment 3 --repo o/r -F=` + badFile,
			"x/y#12 without its full URL (https://github.com/x/y/issues/12)",
		},
		{
			"gh pr create heredoc body",
			"gh pr create --repo o/r --title t --body \"$(cat <<'EOF'\nFixes #13\nEOF\n)\"",
			"#13 without its full URL (https://github.com/o/r/issues/13)",
		},
		{
			"gh issue create $(cat file)",
			`gh issue create --repo o/r --title t --body "$(cat ` + badFile + `)"`,
			"#11 without its full URL (https://github.com/o/r/issues/11)",
		},
	} {
		if d := decide(t, h, dir, c.cmd, nil); d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, c.want) {
			t.Errorf("claude %s: got %+v, want deny containing %q", c.name, d, c.want)
		}
		if d := decideAgyCmd(t, h, dir, c.cmd); d.Decision != decisionDeny || !strings.Contains(d.Reason, c.want) {
			t.Errorf("agy %s: got %+v, want deny containing %q", c.name, d, c.want)
		}
	}

	for _, cmd := range []string{
		`gh pr create --repo o/r --title "feat: x" --body "Closes https://github.com/o/r/issues/7 (o/r#7)"`,
		`gh pr edit o/r#12 --body "See https://github.com/o/r/pull/7#issuecomment-42 and note #3 and timer #4"`,
		`gh issue create --repo o/r --title "bug" --body "Park with ` + "`beekeeper agents park --on o/r#7`" + ` or in session beekeeper#12"`,
		"gh issue comment 7 --repo o/r --body \"```sh\ngh pr view #8 o/r#9\n```\"",
		`gh pr create --repo o/r --title "feat: x" --body-file ` + goodFile,
	} {
		if d := decide(t, h, dir, cmd, nil); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("claude allowed command denied (%s): %s", cmd, d.Reason)
		}
		if d := decideAgyCmd(t, h, dir, cmd); d.Decision == decisionDeny {
			t.Errorf("agy allowed command denied (%s): %s", cmd, d.Reason)
		}
	}
}
