package plugin

import (
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
