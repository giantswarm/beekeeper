package cmd

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/brieflint"
)

func TestDoctorDeskWorkflow(t *testing.T) {
	desk := t.TempDir()
	files := map[string]string{
		"desk/briefs/worker.md":                  "# Worker\nRun `devctl pr merge example/app 1 --update-branch`.\n",
		".claude/skills/register-agent/SKILL.md": "Run beekeeper agents register.\n",
		"skills/merge/SKILL.md":                  "The loop owns every merge.\n",
		"briefs/context.md": "Deployments depend on the database. Read beekeeper:worker-rules.\n" +
			brieflint.QuoteData("Dated order: never run gh pr merge without consent."),
		"memory/orders.md":       "Dated order: never run gh pr merge without consent.\n",
		".git/skills/ignored.md": "Run gh pr merge.\n",
		"skills/ignored.txt":     "Run gh pr merge.\n",
	}
	for name, body := range files {
		path := filepath.Join(desk, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The configured brief is also under the desk: report it only once.
	got := checkDeskWorkflow(desk, []string{filepath.Join(desk, "desk/briefs/worker.md")})
	if len(got) != 3 {
		t.Fatalf("warnings = %v, want three copies", got)
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{"worker.md:2", "register-agent/SKILL.md:1", "merge/SKILL.md:1", "docs/desk-boundary.md"} {
		if !strings.Contains(filepath.ToSlash(all), want) {
			t.Errorf("warnings missing %q: %s", want, all)
		}
	}
	if strings.Contains(all, "example/app") || strings.Contains(all, "--update-branch") {
		t.Fatal("diagnostic exposed desk source text")
	}
	for name, body := range files {
		raw, err := fs.ReadFile(os.DirFS(desk), name)
		if err != nil || string(raw) != body {
			t.Fatalf("doctor changed %s: %v", name, err)
		}
	}
}

func TestDoctorDeskWorkflowConfiguredBriefAndReadErrors(t *testing.T) {
	if got := checkDeskWorkflow("", nil); len(got) != 0 {
		t.Fatalf("unset configuration: %v", got)
	}
	brief := filepath.Join(t.TempDir(), "worker-template")
	if err := os.WriteFile(brief, []byte("Workers must park after pushing.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := checkDeskWorkflow("", []string{brief})
	if len(got) != 1 || !strings.Contains(got[0], brief+":1") {
		t.Fatalf("configured external brief: %v", got)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	got = checkDeskWorkflow(missing, []string{missing + ".md"})
	if len(got) != 2 || !strings.Contains(got[0], "cannot read") || !strings.Contains(got[1], "cannot inspect") {
		t.Fatalf("missing inputs silently skipped: %v", got)
	}
}

func TestDoctorDeskWorkflowReferences(t *testing.T) {
	desk := t.TempDir()
	skill := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(skill, []byte("Run beekeeper agents park.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(desk, "skills")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(skill, filepath.Join(dir, "SKILL.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := checkDeskWorkflow(desk, nil); len(got) != 0 {
		t.Fatalf("installed reference flagged as a copy: %v", got)
	}
}
