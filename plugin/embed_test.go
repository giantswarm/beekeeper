package plugin

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/brieflint"
)

// The shipped skills pass the lint the desks run on theirs: this test is
// the project's CI step for it.
func TestShippedSkillsPassTheLint(t *testing.T) {
	found, err := brieflint.LintFS(Skills, SkillsDir, "plugin")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range found {
		t.Errorf("%s:%d: %s: %s", f.Path, f.Line, f.Rule, f.Text)
	}
}

func TestWorkerRulesIsTheSkillBody(t *testing.T) {
	r := WorkerRules()
	if strings.HasPrefix(r, "---") || strings.Contains(r, "\nname: worker-rules") {
		t.Errorf("frontmatter left in:\n%.200s", r)
	}
	if !strings.HasPrefix(r, "# worker-rules") || !strings.Contains(r, "the supervisor") {
		t.Errorf("not the worker-rules skill:\n%.200s", r)
	}
}

func TestBody(t *testing.T) {
	for in, want := range map[string]string{
		"---\nname: x\n---\n# X\n": "# X\n",
		"# No frontmatter\n":       "# No frontmatter\n",
		"---\nunclosed\n":          "---\nunclosed\n",
	} {
		if got := body(in); got != want {
			t.Errorf("body(%q) = %q, want %q", in, got, want)
		}
	}
}

// No shipped skill suggests a lease wait longer than a foreground command
// runs: a background wait of hours does not outlive a desktop turn, a
// longer wait is lease claim --park.
func TestSkillsSuggestNoLongLeaseWait(t *testing.T) {
	long := regexp.MustCompile(`--wait [0-9]+h|probe timer on .beekeeper lease status`)
	err := fs.WalkDir(Skills, SkillsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := Skills.ReadFile(path)
		if err != nil {
			return err
		}
		if m := long.FindString(string(raw)); m != "" {
			t.Errorf("%s suggests %q", path, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := WorkerRules(); !strings.Contains(r, "--park") {
		t.Errorf("the worker rules name no --park:\n%.400s", r)
	}
}
