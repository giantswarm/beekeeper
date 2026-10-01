package brieflint

import (
	"strings"
	"testing"
	"testing/fstest"
)

const (
	ruleDate  = "date"
	ruleUntil = "until-ships"
)

func TestLintRefusesStaleLines(t *testing.T) {
	for line, want := range map[string]string{
		"- Resumed fully 2026-09-27 16:10 on the person's word.":                ruleDate,
		"No agent polls a rollout (asked 2026-09-26): set a timer.":             ruleDate,
		"until `devctl rollout wait` (devctl#2439) ships, set a timer":          ruleUntil,
		"Use the script until the fix is released.":                             ruleUntil,
		"Merges go through the lane (beekeeper v0.52.0: no clear to wait for).": "fixed-in",
		"Since v0.53.0 the doctor takes you off the roster.":                    "fixed-in",
		"As a workaround, restart the unit.":                                    "workaround",
		"Pin the context for now.":                                              "workaround",
		`The supervisor is "Supervisor run 59"; report to it.`:                  "role-run",
	} {
		got, err := Lint("brief.md", strings.NewReader(line))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Rule != want || got[0].Line != 1 || got[0].Why == "" {
			t.Errorf("Lint(%q) = %+v, want one %s finding", line, got, want)
		}
	}
}

func TestLintPassesCurrentState(t *testing.T) {
	for _, line := range []string{
		"A `hold <repo>` stands until `release <repo>`.",
		"GitHub work stops while the budget refuses and resumes after the reset.",
		"The record is decisions/2026-09-19-0725-adr-fork-line.md.",
		`versionRange: ">=0.0.30-dev.giantswarm.2026-09-19.05-28-28.hea00cfc"`,
		"Files are named `decisions/YYYY-MM-DD-HHMM-<slug>.md`.",
		"An entry whose session no longer runs is replaced.",
		"Report to the supervisor; the guide asks the person.",
		"devctl v8.82.8 or newer merges through the bypass.",
	} {
		got, err := Lint("skill.md", strings.NewReader(line))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("Lint(%q) = %+v, want none", line, got)
		}
	}
}

func TestLintFSWalksMarkdown(t *testing.T) {
	fsys := fstest.MapFS{
		"skills/a/SKILL.md":   {Data: []byte("fine\nuntil X ships\n")},
		"skills/a/notes.txt":  {Data: []byte("2026-09-27 ignored, not Markdown")},
		"skills/.hidden/x.md": {Data: []byte("for now ignored, hidden")},
		"skills/b/SKILL.md":   {Data: []byte("fine\n")},
	}
	got, err := LintFS(fsys, "skills", "desk")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "desk/skills/a/SKILL.md" || got[0].Line != 2 || got[0].Rule != ruleUntil {
		t.Errorf("LintFS = %+v", got)
	}
	// A file named directly is linted whatever its extension.
	if got, _ := LintFS(fsys, "skills/a/notes.txt", ""); len(got) != 1 || got[0].Rule != ruleDate {
		t.Errorf("LintFS(file) = %+v", got)
	}
}
