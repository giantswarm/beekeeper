package brieflint

import (
	"strings"
	"testing"
	"testing/fstest"
)

const (
	ruleDate    = "date"
	ruleUntil   = "until-ships"
	ruleClobber = "clobber"
	ruleFullURL = "full-url"
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
		"Work #7 and report when done.":                                         ruleFullURL,
		"Work o/r#7 and report when done.":                                      ruleFullURL,
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
		"Work o/r#7 (https://github.com/o/r/issues/7) and https://github.com/o/r/pull/8#issuecomment-1.",
		"Work [o/r#7][issue].\n\n[issue]: https://github.com/o/r/issues/7",
		"Wait on note #3, timer #4, `beekeeper agents park --on o/r#7` and session beekeeper#12.",
		"```sh\ngh pr view #7 o/r#8\n```",
		"````markdown\n```sh\ngh issue view #7\n```\nSee o/r#8 inside the outer fence.\n````",
		"Run `gh issue view\n#7` or ``first\no/r#8 second`` in code.",
		"```sh\npattern=\"```\"\n# #7 and o/r#8 stay inside the fence\n```",
		"```sh\ngh issue view #7 at EOF",
		"Theme uses color: #123; and background-color: #123456;.",
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

func TestLintReferenceLinkAndLineNumbers(t *testing.T) {
	doc := "Work [o/r#7][issue].\nAlso fix #8 before closing.\n\n[issue]: https://github.com/o/r/issues/7\n"
	got, err := Lint("brief.md", strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Rule != ruleFullURL || got[0].Line != 2 || !strings.Contains(got[0].Why, "https://github.com/o/r/issues/8") {
		t.Errorf("Lint(reference link doc) = %+v, want one full-url finding on line 2", got)
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

func TestLintRefusesClobberingExamples(t *testing.T) {
	for _, doc := range []string{
		"Capture them apart (`devctl pr merge o/r 1 >out.json 2>gate.log`).",
		"Write the list: `gh pr list --repo o/r > prs.txt`.",
		"Keep the log: `make test 2> \"$log\"`.",
		"Save it: `cat > <file>`.",
		"```bash\nsed 's|a|b|' unit.service > ~/.config/systemd/user/unit.service\n```",
		"```\ncat >/tmp/bk.yaml <<EOF\n```",
	} {
		got, err := Lint("skill.md", strings.NewReader(doc))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Rule != ruleClobber || got[0].Why == "" {
			t.Errorf("Lint(%q) = %+v, want one %s finding", doc, got, ruleClobber)
		}
	}
}

func TestLintPassesSafeRedirects(t *testing.T) {
	for _, doc := range []string{
		"Capture them apart (`devctl pr merge o/r 1 >|out.json 2>|gate.log`).",
		"Never `cmd 2>&1`; quiet it with `cmd >/dev/null 2>&1` or `cmd > /dev/null`.",
		"Append with `echo x >> notes.log`, compare with `test \"$n\" -gt 0` or `n > 0`.",
		"The report is `beekeeper agents idle --done --report \"<report>\"` to `<owner/repo>`.",
		"A count above the floor > 3 goes to the supervisor -> the guide => the person.",
		"`tee >(gzip >|log.gz)`, a `map[string]int` and `x >= y` are fine.",
		"```go\nif a > b {\n```",
		"```bash\nkubectl get pods >| pods.txt\n```",
	} {
		got, err := Lint("skill.md", strings.NewReader(doc))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("Lint(%q) = %+v, want none", doc, got)
		}
	}
}
