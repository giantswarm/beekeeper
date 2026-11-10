package guard

import (
	"strings"
	"testing"
)

func TestReviewerCannotCommitOrPush(t *testing.T) {
	dir := t.TempDir()
	for _, command := range []string{
		plainGitPush, pushOriginHEAD, "git push origin HEAD:refs/heads/feature",
		"git push --repo=https://github.com/o/r.git HEAD:feature", "git push --all",
		"git -C " + dir + " push origin feature", "sh -c 'git push origin feature'",
		"git commit -m fix", "git -c alias.publish='!git push origin feature' publish",
	} {
		t.Run(command, func(t *testing.T) {
			h := Hook{mergeSession: "review-session", ReviewWrite: func(session string) string {
				if session != "review-session" {
					t.Fatalf("wrong session: %q", session)
				}
				return "Refused: forward findings to the worker"
			}}
			if got := h.pushRefusal(command, dir); !strings.Contains(got, "forward findings") {
				t.Fatalf("reviewer command %q: %q", command, got)
			}
		})
	}
	h := Hook{ReviewWrite: func(string) string { t.Fatal("read-only operation checked"); return "" }}
	for _, command := range []string{"git diff", "git show HEAD", "git fetch origin", codexGitStatus} {
		if got := h.pushRefusal(command, dir); got != "" {
			t.Fatal(got)
		}
	}
}
