package guard

import (
	"fmt"
	"testing"
)

func TestUpstreamRepoShortFlagClusters(t *testing.T) {
	u := Upstream{Orgs: []string{allowedOrg}, Git: fakeGit, Getenv: func(string) string { return "" }}
	for _, owner := range []string{allowedOrg, "outside"} {
		for _, flags := range []string{"-R %s/repo", "-R%s/repo", "-xR %s/repo", "-xR%s/repo", "--repo=%s/repo", "-xR=%s/repo"} {
			cmd := "gh pr create " + fmt.Sprintf(flags, owner) + " --title fixture --body fixture"
			t.Run(cmd, func(t *testing.T) {
				got := u.bashWrite(cmd, oursDir)
				if (got != nil) != (owner == "outside") {
					t.Fatalf("write = %+v", got)
				}
			})
		}
	}
	for _, cmd := range []string{
		"gh pr create -tRoutside/repo --body fixture",
		"gh pr create -xt Routside/repo --body fixture",
		"gh pr create --title -xRoutside/repo --body fixture",
	} {
		if got := u.bashWrite(cmd, oursDir); got != nil {
			t.Errorf("%s: title read as repo: %+v", cmd, got)
		}
	}
}
