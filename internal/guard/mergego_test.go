package guard

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMergeRequiresSupervisorGo(t *testing.T) {
	for _, command := range []string{
		"gh pr merge 7 --repo person/project --squash",
		"gh pr merge 7 --repo person/project --squash --match-head-commit 0123456789012345678901234567890123456789",
		"env gh pr merge https://github.com/person/project/pull/7 --squash",
		"bash -c 'gh pr merge 7 --repo person/project --squash'",
		"gh api --method PUT repos/person/project/pulls/7/merge -f sha=0123456789012345678901234567890123456789",
	} {
		if d := decide(t, hook(), "/", command, nil); d == nil || d.PermissionDecision != decisionDeny {
			t.Errorf("merge without supervisor go passed: %s (%+v)", command, d)
		}
	}
}

const mergeGoTestHead = "0123456789012345678901234567890123456789"

func TestMergeGoAcrossHarnesses(t *testing.T) {
	for _, allow := range []bool{false, true} {
		h := hook()
		calls := 0
		h.MergeGo = func(session, repo string, pr int, head string) string {
			calls++
			if repo != "person/project" || pr != 7 || head != mergeGoTestHead {
				t.Errorf("wrong merge tuple: %s %d %s", repo, pr, head)
			}
			if !allow {
				return "no go from the supervisor recorded on disk"
			}
			return ""
		}
		for _, command := range []string{
			"gh pr merge 7 --repo person/project --squash --match-head-commit " + mergeGoTestHead,
			"gh pr merge 7 --repo person/project --squash --match-head-commit " + mergeGoTestHead + " 2>&1 | tee result.log",
			"gh pr merge https://github.com/person/project/pull/7 --squash --match-head-commit=" + mergeGoTestHead,
			"bash -c 'gh pr merge 7 -R person/project --match-head-commit " + mergeGoTestHead + "'",
			"gh api --method PUT repos/person/project/pulls/7/merge -f sha=" + mergeGoTestHead,
			"gh api 2>/dev/null -X PUT repos/person/project/pulls/7/merge -f sha=" + mergeGoTestHead,
			"gh api 2>&1 -X PUT repos/person/project/pulls/7/merge -f sha=" + mergeGoTestHead,
			"gh -i api -X PUT repos/person/project/pulls/7/merge -f sha=" + mergeGoTestHead,
			"gh -X PUT api repos/person/project/pulls/7/merge -f sha=" + mergeGoTestHead,
			"gh pr merge 7 -R person/project --match-head-commit 2>/dev/null " + mergeGoTestHead,
		} {
			d := decide(t, h, "/", command, nil)
			denied := d != nil && d.PermissionDecision == decisionDeny
			if denied == allow {
				t.Errorf("Claude allow=%v: %s: %+v", allow, command, d)
			}
			agy := decideAgyCmd(t, h, "/", command)
			if (agy.Decision == decisionDeny) == allow {
				t.Errorf("agy allow=%v: %s: %+v", allow, command, agy)
			}
			codex := decideCodexCmd(t, h, "/", command)
			if (codex.PermissionDecision == decisionDeny) == allow {
				t.Errorf("Codex allow=%v: %s: %+v", allow, command, codex)
			}
		}
		if calls < 12 {
			t.Errorf("only %d authorization checks", calls)
		}
	}
}

func TestMergeGoRejectsUnpinnedAndDeferredMerges(t *testing.T) {
	h := hook()
	h.MergeGo = func(string, string, int, string) string { t.Error("unsafe merge reached authority"); return "" }
	for _, command := range []string{
		"gh pr merge 7 -R person/project",
		"gh pr merge 7 -R person/project --match-head-commit abcdef0",
		"gh pr merge 7 -R person/project --auto --match-head-commit " + mergeGoTestHead,
		"gh api -X PUT repos/person/project/pulls/7/merge --input body.json",
		"gh api -X PUT repos/person/project/pulls/7/merge -f sha=" + mergeGoTestHead + " -f sha=" + mergeGoTestHead,
		"gh api graphql -f 'query=mutation { mergePullRequest(input: {}) { clientMutationId } }'",
	} {
		if d := decide(t, h, "/", command, nil); d == nil || d.PermissionDecision != decisionDeny {
			t.Errorf("unsafe merge passed: %s", command)
		}
	}
	for _, command := range []string{"gh pr view 7 -R person/project", "gh api repos/person/project/pulls/7/merge", "echo 'gh pr merge 7'", "gh issue comment 1 -R person/project --body 'gh pr merge 7'"} {
		if d := decide(t, h, "/", command, nil); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("non-merge refused: %s: %+v", command, d)
		}
	}
}

func TestMergeGoCannotHideInAnExecutedScript(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "merge.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ngh pr merge 7 -R person/project\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"bash " + script, script, "python3 -c 'import subprocess; subprocess.run([\"gh\",\"pr\",\"merge\",\"7\"])'"} {
		if d := decide(t, hook(), dir, command, nil); d == nil || d.PermissionDecision != decisionDeny {
			t.Errorf("hidden merge passed: %s (%+v)", command, d)
		}
	}
}

func TestReviewMergeGoAPIArgumentBypasses(t *testing.T) {
	for _, command := range []string{
		"gh api 2>/dev/null -X PUT repos/person/project/pulls/7/merge",
		"gh api 2>&1 -X PUT repos/person/project/pulls/7/merge",
		"gh -i api -X PUT repos/person/project/pulls/7/merge",
		"gh -X PUT api repos/person/project/pulls/7/merge",
	} {
		h := hook()
		for _, cmd := range []string{command, command + " -f sha=" + mergeGoTestHead} {
			if d := decide(t, h, "/", cmd, nil); d == nil || d.PermissionDecision != decisionDeny {
				t.Errorf("Claude bypass: %s (%+v)", cmd, d)
			}
			if d := decideAgyCmd(t, h, "/", cmd); d.Decision != decisionDeny {
				t.Errorf("agy bypass: %s (%+v)", cmd, d)
			}
			if d := decideCodexCmd(t, h, "/", cmd); d.PermissionDecision != decisionDeny {
				t.Errorf("Codex bypass: %s (%+v)", cmd, d)
			}
		}
	}
}

func TestReviewMergeGoShellProse(t *testing.T) {
	dir := t.TempDir()
	for i, script := range []string{
		"# gh pr merge 7 -R person/project\necho ok\n",
		"# gh api -X PUT repos/person/project/pulls/7/merge\necho ok\n",
		"bash -c '# gh pr merge 7 -R person/project\necho ok'\n",
		"echo 'gh pr merge 7 -R person/project'\n",
		"echo 'gh api -X PUT repos/person/project/pulls/7/merge'\n",
		"cat <<'DOC'\ngh pr merge 7 -R person/project\ngh api -X PUT repos/person/project/pulls/7/merge\nDOC\n",
	} {
		file := filepath.Join(dir, fmt.Sprintf("prose-%d.sh", i))
		if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, command := range []string{"bash " + file, "bash <<'SCRIPT'\n" + script + "SCRIPT\n"} {
			if d := decide(t, hook(), dir, command, nil); d != nil && d.PermissionDecision == decisionDeny {
				t.Errorf("prose refused: %s (%+v)", command, d)
			}
		}
	}
}

func TestReviewMergeGoExecutedScriptsStillRequireDirectMerges(t *testing.T) {
	dir := t.TempDir()
	h := hook()
	h.MergeGo = func(string, string, int, string) string { return "" }
	for i, script := range []string{
		"gh pr merge 7 -R person/project --match-head-commit " + mergeGoTestHead + "\n",
		"gh api 2>/dev/null -X PUT repos/person/project/pulls/7/merge -f sha=" + mergeGoTestHead + "\n",
		"gh -X PUT api repos/person/project/pulls/7/merge -f sha=" + mergeGoTestHead + "\n",
	} {
		file := filepath.Join(dir, fmt.Sprintf("actual-%d.sh", i))
		if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, command := range []string{"bash " + file, "bash <<'SCRIPT'\n" + script + "SCRIPT\n"} {
			if d := decide(t, h, dir, command, nil); d == nil || d.PermissionDecision != decisionDeny {
				t.Errorf("hidden merge passed: %s (%+v)", command, d)
			}
		}
	}
}
