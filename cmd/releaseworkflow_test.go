package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// decideStep is the auto-release workflow's tag decision, as it ships.
func decideStep(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "zz_generated.auto_release.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct{ Name, Run string }
		}
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	for _, s := range workflow.Jobs["tag"].Steps {
		if s.Name == "Decide whether to tag" {
			return s.Run
		}
	}
	t.Fatal("the workflow has no step deciding the tag")
	return ""
}

// A fix after a stable release is its next patch's candidate, which only the
// promotion tags stable. The workflow is generated: an alignment that brings
// back the patch fast path fails here, and the edit is restored.
func TestAFixAfterAStableIsTheNextPatchsCandidate(t *testing.T) {
	step := decideStep(t)
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", args...) //nolint:gosec // the test's fixed commands
		c.Dir = repo
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	decide := func(want, promote string) string {
		t.Helper()
		output := filepath.Join(t.TempDir(), "output")
		context := filepath.Join(t.TempDir(), "context.json")
		commit := git("rev-parse", "HEAD")
		if err := os.WriteFile(context, []byte(`[{"commits":[{"id":"`+commit+`"}]}]`), 0o600); err != nil {
			t.Fatal(err)
		}
		c := exec.Command("bash", "-c", step) //nolint:gosec // the workflow's own step
		c.Dir = repo
		c.Env = append(os.Environ(), "NEXT=v1.2.4", "WANT="+want, "PROMOTE="+promote, "CLIFF_CONTEXT="+context, "GITHUB_OUTPUT="+output, "GITHUB_REF_NAME=main")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("decide: %v\n%s", err, out)
		}
		raw, err := os.ReadFile(output) //nolint:gosec // the test's file
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	git("init", "-b", "main")
	git("commit", "--allow-empty", "-m", "feat: first")
	git("tag", "v1.2.3")
	git("commit", "--allow-empty", "-m", "fix: after the stable")
	if out := decide("auto", ""); !strings.Contains(out, "tag=v1.2.4-rc.1\n") || !strings.Contains(out, "prerelease=true\n") {
		t.Fatalf("a fix after v1.2.3 decided:\n%s", out)
	}
	git("tag", "v1.2.4-rc.1")
	if out := decide("stable", "v1.2.4-rc.1"); !strings.Contains(out, "tag=v1.2.4\n") || !strings.Contains(out, "prerelease=false\n") {
		t.Fatalf("the promotion decided:\n%s", out)
	}
}
