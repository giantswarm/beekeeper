package guard

import (
	"strings"
	"testing"
)

// The admin flag changes GitHub protection behavior, never desk authority.
// Exercise each harness's wire format, including the REST merge path.
func TestAdminAndAPIMergesRequireExactHeadGoAcrossHarnesses(t *testing.T) {
	for _, head := range []string{mergeGoTestHead, strings.Repeat("a", 40)} {
		for _, granted := range []bool{false, true} {
			h := hook()
			calls := 0
			h.MergeGo = func(_, repo string, pr int, gotHead string) string {
				calls++
				if repo != mergeGoTestRepo || pr != 7 {
					t.Fatalf("wrong merge target: %s %d", repo, pr)
				}
				if !granted || gotHead != mergeGoTestHead {
					return "no exact-head supervisor go"
				}
				return ""
			}
			for _, command := range []string{
				"gh pr merge 7 --repo person/project --admin --squash --match-head-commit " + head,
				"gh api --method PUT repos/person/project/pulls/7/merge -f sha=" + head,
			} {
				wantDeny := !granted || head != mergeGoTestHead
				d := decide(t, h, "/", command, nil)
				if got := d != nil && d.PermissionDecision == decisionDeny; got != wantDeny {
					t.Errorf("Claude granted=%v: %s: %+v", granted, command, d)
				}
				if d := decideAgyCmd(t, h, "/", command); (d.Decision == decisionDeny) != wantDeny {
					t.Errorf("agy granted=%v: %s: %+v", granted, command, d)
				}
				if d := decideCodexCmd(t, h, "/", command); (d.PermissionDecision == decisionDeny) != wantDeny {
					t.Errorf("Codex granted=%v: %s: %+v", granted, command, d)
				}
			}
			if calls != 6 {
				t.Errorf("authorization calls = %d, want 6", calls)
			}
		}
	}
}
