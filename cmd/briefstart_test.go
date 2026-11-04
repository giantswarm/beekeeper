package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/brieflint"
	"github.com/giantswarm/beekeeper/internal/config"
)

func TestReadBriefLint(t *testing.T) {
	for _, text := range []string{
		"\n# Task\nUse a workaround until the fix ships.\n",
		"# Task\nRun `echo result > result.txt`.\n",
		"# Task\nSee #123.\n",
	} {
		t.Run(text, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "brief.md")
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			findings, err := brieflint.Lint(path, strings.NewReader(text))
			if err != nil || len(findings) == 0 {
				t.Fatalf("lint = %v, %v", findings, err)
			}
			brief, err := readBrief(path)
			if err == nil || Code(err) != ExitRefused || brief != "" {
				t.Fatalf("readBrief = %q, %v; want refusal", brief, err)
			}
			for _, f := range findings {
				if !strings.Contains(err.Error(), f.Rule+": "+f.Why) || !strings.Contains(err.Error(), f.Text) {
					t.Errorf("missing finding %+v in %v", f, err)
				}
			}
			if strings.HasPrefix(text, "\n") && !strings.Contains(err.Error(), path+":3:") {
				t.Errorf("original line number missing: %v", err)
			}
		})
	}
}

func TestReadBriefClean(t *testing.T) {
	const text = "# Count lines\n\nRun `wc -l README.md` and report to the supervisor."
	path := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readBrief(path)
	if err != nil || got != text {
		t.Fatalf("readBrief = %q, %v; want unchanged brief", got, err)
	}
}

func TestAgentsStartRefusesLintFinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(path, []byte("# Task\nUse a workaround.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{}}
	c := a.agentStartCmd()
	c.SetArgs([]string{"--harness=codex", "--model=gpt-5", "--force", workerRole, path})
	err := c.Execute()
	if err == nil || Code(err) != ExitRefused || !strings.Contains(err.Error(), path+":2: workaround:") {
		t.Fatalf("agents start = %v; want brief lint refusal even with --force", err)
	}
}
