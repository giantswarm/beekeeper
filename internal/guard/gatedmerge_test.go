package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const gatedCommand = "devctl pr merge giantswarm/project 7 --update-branch"

// gatedHook is a desk whose giantswarm merges are the gate's and whose
// supervisor go is recorded for every merge it is asked about.
func gatedHook(t *testing.T) Hook {
	h := hook()
	h.GatedOwners = []string{"giantswarm"}
	h.MergeGo = func(_, repo string, _ int, _ string) string {
		if strings.HasPrefix(repo, "giantswarm/") {
			t.Errorf("a gated repository's merge reached the go check: %s", repo)
		}
		return ""
	}
	return h
}

func TestGatedRepositoryMergeIsTheGates(t *testing.T) {
	h := gatedHook(t)
	for _, command := range []string{
		"gh pr merge 7 --repo giantswarm/project --squash --match-head-commit " + mergeGoTestHead,
		"gh pr merge 7 -R GiantSwarm/project --squash --match-head-commit " + mergeGoTestHead,
		"gh pr merge https://github.com/giantswarm/project/pull/7 --squash --match-head-commit=" + mergeGoTestHead,
		"GH_REPO=giantswarm/project gh pr merge 7 --squash --match-head-commit " + mergeGoTestHead,
		"gh pr merge 7 --repo giantswarm/project --auto",
		"gh api -X PUT repos/giantswarm/project/pulls/7/merge -f sha=" + mergeGoTestHead,
		"gh api --method PUT /repos/giantswarm/project/pulls/7/merge -f sha=" + mergeGoTestHead + " -f merge_method=squash",
		"gh api 2>/dev/null -X PUT https://api.github.com/repos/giantswarm/project/pulls/7/merge -f sha=" + mergeGoTestHead,
		"curl -X PUT -H 'Authorization: Bearer x' https://api.github.com/repos/giantswarm/project/pulls/7/merge -d '{\"sha\":\"" + mergeGoTestHead + "\"}'",
		"curl --request=PUT --url=https://api.github.com/repos/giantswarm/project/pulls/7/merge",
		"bash -c 'gh pr merge 7 -R giantswarm/project --match-head-commit " + mergeGoTestHead + "'",
	} {
		for harness, reason := range map[string]string{
			"Claude": func() string {
				d := decide(t, h, "/", command, nil)
				if d == nil || d.PermissionDecision != decisionDeny {
					return ""
				}
				return d.Reason
			}(),
			"agy": func() string {
				d := decideAgyCmd(t, h, "/", command)
				if d.Decision != decisionDeny {
					return ""
				}
				return d.Reason
			}(),
			"Codex": func() string {
				d := decideCodexCmd(t, h, "/", command)
				if d.PermissionDecision != decisionDeny {
					return ""
				}
				return d.Reason
			}(),
		} {
			if !strings.Contains(reason, gatedCommand) && !strings.Contains(reason, "devctl pr merge GiantSwarm/project 7 --update-branch") {
				t.Errorf("%s: %s: want a refusal naming %q, got %q", harness, command, gatedCommand, reason)
			}
		}
	}
}

func TestGatedMergeNamesTheGateWhereTheRepositoryIsUnread(t *testing.T) {
	h := gatedHook(t)
	for _, command := range []string{
		"gh api graphql -f 'query=mutation { mergePullRequest(input: {pullRequestId: \"PR_x\"}) { clientMutationId } }'",
		"curl -X POST https://api.github.com/graphql -d '{\"query\":\"mutation { mergePullRequest(input: {}) { clientMutationId } }\"}'",
		"gh api -X POST repos/giantswarm/project/merges -f base=main -f head=feature",
		"curl -X POST https://api.github.com/repos/giantswarm/project/merges -d '{\"base\":\"main\",\"head\":\"feature\"}'",
	} {
		d := decide(t, h, "/", command, nil)
		if d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "devctl pr merge") {
			t.Errorf("merge passed or named no gated command: %s (%+v)", command, d)
		}
	}
}

func TestGatedMergeCannotHideInAProgram(t *testing.T) {
	dir := t.TempDir()
	for i, code := range []string{
		"import requests\nrequests.put('https://api.github.com/repos/giantswarm/project/pulls/7/merge', json={'sha': 'x'})\n",
		"import requests\nrepo = 'giantswarm/project'\nrequests.put('https://api.github.com/repos/' + repo + '/pulls/7' + '/merge')\n",
		"import requests\nrequests.post('https://api.github.com/graphql', json={'query': 'mutation { mergePullRequest(input: {}) { clientMutationId } }'})\n",
	} {
		file := filepath.Join(dir, "merge"+string(rune('a'+i))+".py")
		if err := os.WriteFile(file, []byte(code), 0o600); err != nil {
			t.Fatal(err)
		}
		if d := decide(t, gatedHook(t), dir, "python3 "+file, nil); d == nil || d.PermissionDecision != decisionDeny {
			t.Errorf("program merge passed: %q (%+v)", code, d)
		}
	}
	script := filepath.Join(dir, "merge.sh")
	if err := os.WriteFile(script, []byte("curl -X PUT https://api.github.com/repos/giantswarm/project/pulls/7/merge\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d := decide(t, gatedHook(t), dir, "bash "+script, nil); d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, gatedCommand) {
		t.Errorf("script merge passed or named no gated command: %+v", d)
	}
}

func TestPersonalRepositoryMergeStaysWithTheGo(t *testing.T) {
	h := gatedHook(t)
	calls := 0
	h.MergeGo = func(_, repo string, pr int, head string) string {
		calls++
		if repo != "teemow/project" || pr != 7 || head != mergeGoTestHead {
			t.Errorf("wrong merge tuple: %s %d %s", repo, pr, head)
		}
		return ""
	}
	command := "gh pr merge 7 --repo teemow/project --squash --match-head-commit " + mergeGoTestHead
	if d := decide(t, h, "/", command, nil); d != nil && d.PermissionDecision == decisionDeny {
		t.Errorf("Claude refused a personal repository's merge on its go: %+v", d)
	}
	if d := decideCodexCmd(t, h, "/", command); d.PermissionDecision == decisionDeny {
		t.Errorf("Codex refused a personal repository's merge on its go: %+v", d)
	}
	if d := decideAgyCmd(t, h, "/", command); d.Decision == decisionDeny {
		t.Errorf("agy refused a personal repository's merge on its go: %+v", d)
	}
	if calls != 3 {
		t.Errorf("go checked %d times, want 3", calls)
	}
	for _, command := range []string{
		"curl -X PUT https://api.github.com/repos/teemow/project/pulls/7/merge",
		"gh pr merge 7 --repo teemow/project --squash",
	} {
		if d := decide(t, h, "/", command, nil); d == nil || d.PermissionDecision != decisionDeny {
			t.Errorf("unpinned or curl merge passed: %s", command)
		}
	}
}

func TestGatedMergeReadsPass(t *testing.T) {
	h := gatedHook(t)
	for _, command := range []string{
		"gh pr view 7 -R giantswarm/project",
		"gh api repos/giantswarm/project/pulls/7/merge",
		"curl -s https://api.github.com/repos/giantswarm/project/pulls/7/merge",
		"gh pr list -R giantswarm/project --search 'is:merged'",
	} {
		if d := decide(t, h, "/", command, nil); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("read refused: %s: %+v", command, d)
		}
	}
}
