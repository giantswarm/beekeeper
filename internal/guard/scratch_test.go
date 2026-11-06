package guard

import (
	"path/filepath"
	"testing"
)

// A command under `beekeeper sandbox scratch` runs on the host, so the hook
// holds it to the rules of the same command written bare.
func TestSandboxScratchCommandIsChecked(t *testing.T) {
	repo := initGitRepo(t, "feat-x", "https://github.com/giantswarm/beekeeper.git")
	h := Hook{
		Self:        testBeekeeperExe,
		Shell:       zshCmd,
		SecretFiles: []string{filepath.Join(repo, "*.secret.yaml")},
		Upstream:    Upstream{Orgs: []string{allowedOrg}},
	}
	for _, inner := range []string{
		"devctl pr merge giantswarm/beekeeper 7",
		"gh pr merge 7 --admin",
		"git push --force origin feat-x",
		"cat " + filepath.Join(repo, "a.secret.yaml"),
	} {
		for _, wrapper := range []string{
			"beekeeper sandbox scratch -- ",
			"beekeeper sandbox scratch ",
			"beekeeper --config /srv/scratch.yaml sandbox scratch -- ",
			"env beekeeper sandbox scratch -- env ",
		} {
			if d := decide(t, h, repo, wrapper+inner, nil); d == nil || d.PermissionDecision != decisionDeny {
				t.Errorf("%s%s: got %+v, want deny", wrapper, inner, d)
			}
		}
	}
	if scratchWords([]string{"sandbox", "broker"}) != 0 || scratchWords([]string{executionRunCommand, "--", "sandbox", "scratch"}) != 0 {
		t.Error("another beekeeper call taken for a scratch run")
	}
}
