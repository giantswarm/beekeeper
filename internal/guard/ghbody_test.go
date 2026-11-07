package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGHBodyFullURLCheckClaudeAndAgy(t *testing.T) {
	const (
		wantRef11       = "#11 without its full URL (https://github.com/o/r/issues/11)"
		wantUninspected = "cannot inspect the body"
	)
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
			wantRef11,
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
			wantRef11,
		},
		{
			"printf piped to --body-file -",
			`printf '%s' 'Closes #7' | gh pr create -R o/r --title t --body-file -`,
			"#7 without its full URL (https://github.com/o/r/issues/7)",
		},
		{
			"echo piped to -F -",
			`echo 'Fixes #14' | gh issue comment 1 -R o/r -F -`,
			"#14 without its full URL (https://github.com/o/r/issues/14)",
		},
		{
			"cat file piped to --body-file -",
			`cat ` + badFile + ` | gh pr create -R o/r --title t --body-file -`,
			wantRef11,
		},
		{
			"input redirection with -F -",
			`gh pr create -R o/r --title t -F - < ` + badFile,
			wantRef11,
		},
		{
			"here-string with -F -",
			`gh pr create -R o/r --title t -F - <<< 'Closes #15'`,
			"#15 without its full URL (https://github.com/o/r/issues/15)",
		},
		{
			"file written earlier in same command",
			`printf '%s' 'Closes #16' >| staged.md && gh pr create -R o/r --title t --body-file staged.md`,
			"#16 without its full URL (https://github.com/o/r/issues/16)",
		},
		{
			"file written earlier by heredoc in same command",
			"cat <<'EOF' >| staged-hd.md\nCloses #17\nEOF\ngh issue create -R o/r --title t -F staged-hd.md",
			"#17 without its full URL (https://github.com/o/r/issues/17)",
		},
		{
			"resolved body variable with unlinked ref",
			`BODY='Closes #18'; gh pr create -R o/r --title t --body "$BODY"`,
			"#18 without its full URL (https://github.com/o/r/issues/18)",
		},
		{
			"unreadable --body-file",
			`gh pr create -R o/r --title t --body-file missing.md`,
			wantUninspected,
		},
		{
			"unreadable input redirection with -F -",
			`gh pr create -R o/r --title t -F - < missing.md`,
			wantUninspected,
		},
		{
			"unreadable $(cat missing.md)",
			`gh pr create -R o/r --title t --body "$(cat missing.md)"`,
			wantUninspected,
		},
		{
			"uninspectable writer to file in same command",
			`unknown_gen >| gen.md && gh pr create -R o/r --title t --body-file gen.md`,
			wantUninspected,
		},
		{
			"uninspectable pipeline to --body-file -",
			`unknown_gen | gh pr create -R o/r --title t --body-file -`,
			wantUninspected,
		},
		{
			"bare --body-file - without stdin",
			`gh pr create -R o/r --title t --body-file -`,
			wantUninspected,
		},
		{
			"unresolved body variable",
			`gh pr create -R o/r --title t --body "$UNRESOLVED_BODY"`,
			wantUninspected,
		},
		{
			"unresolved variable in here-string",
			`gh pr create -R o/r --title t -F - <<< "$UNRESOLVED_BODY"`,
			wantUninspected,
		},
		{
			"unresolved variable in unquoted heredoc",
			"gh pr create -R o/r --title t -F - <<EOF\n$UNRESOLVED_BODY\nEOF",
			wantUninspected,
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
		`gh issue edit o/r#12 --body "See https://github.com/o/r/pull/7#issuecomment-42 and note #3 and timer #4"`,
		`gh issue create --repo o/r --title "bug" --body "Park with ` + "`beekeeper agents park --on o/r#7`" + ` or in session beekeeper#12"`,
		"gh issue comment 7 --repo o/r --body \"```sh\ngh pr view #8 o/r#9\n```\"",
		"gh issue comment 7 --repo o/r --body \"````markdown\n```sh\ngh pr view #8\n```\no/r#9\n````\"",
		"gh issue comment 7 --repo o/r --body \"in `multiline\n#8 span` and color: #123; background-color: #123456;\"",
		`gh pr create --repo o/r --title "feat: x" --body-file ` + goodFile,
		`printf '%s' 'Closes https://github.com/o/r/issues/7' | gh pr create -R o/r --title t --body-file -`,
		`gh pr create -R o/r --title t -F - < ` + goodFile,
		`gh pr create -R o/r --title t -F - <<< 'Closes https://github.com/o/r/issues/7'`,
		`printf '%s' 'Closes https://github.com/o/r/issues/7' >| staged-ok.md && gh pr create -R o/r --title t -F staged-ok.md`,
		`BODY='Closes https://github.com/o/r/issues/7'; gh pr create -R o/r --title t --body "$BODY"`,
	} {
		if d := decide(t, h, dir, cmd, nil); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("claude allowed command denied (%s): %s", cmd, d.Reason)
		}
		if d := decideAgyCmd(t, h, dir, cmd); d.Decision == decisionDeny {
			t.Errorf("agy allowed command denied (%s): %s", cmd, d.Reason)
		}
	}
}

func TestGHBodyUnreadableFileExplainsEarlierToolCall(t *testing.T) {
	h := Hook{Self: testBeekeeperExe, Shell: testShell}
	cmd := `gh pr create -R o/r --title t --body-file missing.md`
	const want = "write the file in an earlier tool call"
	if d := decide(t, h, t.TempDir(), cmd, nil); d == nil || !strings.Contains(d.Reason, want) {
		t.Fatalf("got %+v, want refusal containing %q", d, want)
	}
	if d := decideAgyCmd(t, h, t.TempDir(), cmd); !strings.Contains(d.Reason, want) {
		t.Fatalf("agy got %+v, want refusal containing %q", d, want)
	}
}

func TestGHPRCreateRefusesDuplicateIssuePullRequest(t *testing.T) {
	dir := t.TempDir()
	h := Hook{
		Self:  testBeekeeperExe,
		Shell: testShell,
		PRCreate: func(_, repo, head, title, body string) string {
			if repo == "o/r" && head != "own-branch" && strings.Contains(title+"\n"+body, "https://github.com/o/r/issues/7") {
				return "o/r#7 already has an open pull request https://github.com/o/r/pull/42"
			}
			return ""
		},
	}
	cmd := `gh pr create --repo o/r --head other-branch --title "fix: issue 7" --body "Closes https://github.com/o/r/issues/7"`
	const want = "o/r#7 already has an open pull request https://github.com/o/r/pull/42"
	if d := decide(t, h, dir, cmd, nil); d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, want) {
		t.Fatalf("claude got %+v, want deny containing %q", d, want)
	}
	draftCmd := `gh pr create --draft --repo o/r --head other-branch --title "fix: issue 7" --body "Closes https://github.com/o/r/issues/7"`
	if d := decide(t, h, dir, draftCmd, nil); d != nil && d.PermissionDecision == decisionDeny {
		t.Fatalf("draft PR create should not be refused: %+v", d)
	}
	ownCmd := `gh pr create --repo o/r --head own-branch --title "fix: issue 7" --body "Closes https://github.com/o/r/issues/7"`
	if d := decide(t, h, dir, ownCmd, nil); d != nil && d.PermissionDecision == decisionDeny {
		t.Fatalf("own-branch PR create should not be refused: %+v", d)
	}
	for _, bypassCmd := range []string{
		`gh pr create --repo o/r --fill`,
		`gh pr create --repo o/r --fill-first`,
		`gh pr create --repo o/r --fill-verbose`,
		`gh pr create --repo o/r -f`,
		`gh pr create --repo o/r --web`,
		`gh pr create --repo o/r --title "fix: issue 7"`,
		`gh pr create --repo o/r --body "Closes https://github.com/o/r/issues/7"`,
		`gh pr create --repo o/r --title "$UNRESOLVED_TITLE" --body "See https://github.com/o/r/issues/7"`,
		`gh pr create --repo o/r --head "$UNRESOLVED_HEAD" --title "fix: issue 7" --body "Closes https://github.com/o/r/issues/7"`,
	} {
		if d := decide(t, h, dir, bypassCmd, nil); d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "cannot inspect the body") {
			t.Fatalf("bypass command %q should be refused as uninspectable, got %+v", bypassCmd, d)
		}
	}
}

func TestReview3325EffectiveDraftFlags(t *testing.T) {
	h := Hook{Self: testBeekeeperExe, Shell: testShell, PRCreate: func(_, _, _, _, _ string) string { return "duplicate fixture" }}
	for _, flags := range []string{"--draft --draft=false", "-d -d=false", "--draft=true -d=false", "-d=true --draft=0", "--draft=false", "-d=false", "--draft -dd=false"} {
		for _, metadata := range []string{`--title t --body 'Closes https://github.com/o/r/issues/55'`, "--fill"} {
			command := "gh pr create -R o/r " + flags + " " + metadata
			if d := decide(t, h, t.TempDir(), command, nil); d == nil || d.PermissionDecision != decisionDeny {
				t.Errorf("admitted non-draft %s: %+v", command, d)
			}
			if d := decideAgyCmd(t, h, t.TempDir(), command); d.Decision != decisionDeny {
				t.Errorf("agy admitted non-draft %s: %+v", command, d)
			}
		}
	}
	for _, flags := range []string{"--draft=false --draft", "-d=false -d", "--draft=true", "-d=1"} {
		if d := decide(t, h, t.TempDir(), "gh pr create -R o/r "+flags+" --fill", nil); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("denied effective draft %s: %+v", flags, d)
		}
	}
}
