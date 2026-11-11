package loopworkflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixtureRawWorkflowReads(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"zz_generated.auto_release.yaml", "auto-release.yaml", "future-release.yml"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			gh := filepath.Join(dir, "gh")
			if err := os.Symlink(self, gh); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, pullJSONFile), []byte(`{}`), 0600); err != nil {
				t.Fatal(err)
			}
			for _, extra := range [][]string{
				nil,
				{ghInclude},
				{ghInclude, "-H", `If-None-Match: "fixture"`},
			} {
				args := append([]string{ghAPI, "-H", acceptRawHeader, "repos/fixture/project/contents/.github/workflows/" + name + "?ref=main"}, extra...)
				cmd := exec.CommandContext(t.Context(), gh, args...) //nolint:gosec // isolated fixture executable
				cmd.Env = append(os.Environ(), "FIXTURE_DIR="+dir)
				out, err := cmd.CombinedOutput()
				if err == nil || !strings.Contains(string(out), "HTTP 404") {
					t.Fatalf("missing workflow must answer GitHub 404: %s, %v", out, err)
				}
				if _, err := os.Stat(filepath.Join(dir, "unknown-call")); !os.IsNotExist(err) {
					t.Fatalf("workflow read recorded an unknown call: %v", err)
				}
			}
		})
	}
}

func TestFixtureWorkflowReadScope(t *testing.T) {
	for _, endpoint := range []string{
		"repos/other/project/contents/.github/workflows/release.yml?ref=main",
		"repos/fixture/project/contents/README.md?ref=main",
		"repos/fixture/project/contents/.github/workflows/../README.md?ref=main",
		"repos/fixture/project/contents/.github/workflows/%2e%2e?ref=main",
		"repos/fixture/project/contents/.github/workflows/release.yml?ref=change",
	} {
		if fixtureWorkflowRead(endpoint) {
			t.Errorf("unrelated endpoint accepted: %s", endpoint)
		}
	}
}

func TestUnknownFixtureCallPreservesFirstFailure(t *testing.T) {
	dir := t.TempDir()
	first := []byte(`["api","unsupported-first-operation"]`)
	recordUnknownCall(dir, first)
	recordUnknownCall(dir, []byte(`["api","follow-up-operation"]`))
	got, err := os.ReadFile(filepath.Join(dir, "unknown-call")) //nolint:gosec // isolated temporary fixture
	if err != nil || string(got) != string(first) {
		t.Fatalf("first failure overwritten: %s, %v", got, err)
	}
}
