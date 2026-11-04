package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
)

// A finding warns and never refuses: reporter turns and the supervisor's
// generated briefs read the same file path.
func TestBriefFindingsWarnWithoutRefusing(t *testing.T) {
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
			if brief, err := readBrief(path); err != nil || brief != strings.TrimSpace(text) {
				t.Fatalf("readBrief = %q, %v; want the brief", brief, err)
			}
			var out bytes.Buffer
			warnBriefLint(&out, path)
			if !strings.Contains(out.String(), "brief lint: "+path+":") {
				t.Fatalf("warning = %q", out.String())
			}
			if strings.HasPrefix(text, "\n") && !strings.Contains(out.String(), path+":3:") {
				t.Errorf("original line number missing: %q", out.String())
			}
		})
	}
}

func TestCleanBriefPrintsNoWarning(t *testing.T) {
	const text = "# Count lines\n\nRun `wc -l README.md` and report to the supervisor."
	path := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	warnBriefLint(&out, path)
	if got, err := readBrief(path); err != nil || got != text || out.Len() != 0 {
		t.Fatalf("readBrief = %q, %v; warning %q", got, err, out.String())
	}
}

// A relay's brief names the successor's run, which the lint's role-run rule
// flags. A hand-over must survive that, as a file too.
func TestRelayBriefIsNeverRefused(t *testing.T) {
	text := guideRole.successorBrief(config.Role{Skill: guideName}, guideRole.runName(12), guideRole.runName(11)) + "\nGuide run 12 takes over.\n"
	path := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBrief(path); err != nil {
		t.Fatalf("relay brief refused: %v", err)
	}
	var out bytes.Buffer
	warnBriefLint(&out, path)
	if !strings.Contains(out.String(), "role-run") {
		t.Fatalf("warning = %q, want the role-run finding", out.String())
	}
}
