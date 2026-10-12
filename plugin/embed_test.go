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

// The worker rules name every topic skill, each topic is a skill of its
// own name, and none restates the rules' heading.
func TestWorkerRulesNameTheTopics(t *testing.T) {
	r := WorkerRules()
	for _, topic := range Topics {
		raw := string(SkillFile(topic))
		if !strings.Contains(r, "`beekeeper:"+topic+"`") {
			t.Errorf("the worker rules do not name beekeeper:%s", topic)
		}
		if !strings.HasPrefix(raw, "---\nname: "+topic+"\n") || !strings.HasPrefix(Skill(topic), "# "+topic+" — ") {
			t.Errorf("%s/SKILL.md is not the %s skill:\n%.200s", topic, topic, raw)
		}
	}
}

// The budgets a worker's context keeps, in bytes at the measured 2.7 bytes
// per token of this text: the core rules at most 1,000 tokens, a topic
// skill at most 700, its description at most 60.
func TestWorkerRulesBudget(t *testing.T) {
	const bytesPerToken = 2.7
	if n := len(WorkerRules()); n > 1000*bytesPerToken {
		t.Errorf("the worker rules have %d bytes, more than 1,000 tokens", n)
	}
	for _, topic := range Topics {
		raw := string(SkillFile(topic))
		if len(raw) > 700*bytesPerToken {
			t.Errorf("%s has %d bytes, more than 700 tokens", topic, len(raw))
		}
		desc, _, _ := strings.Cut(raw[strings.Index(raw, "\ndescription: ")+len("\ndescription: "):], "\n")
		if len(desc) > 60*bytesPerToken {
			t.Errorf("%s's description has %d bytes, more than 60 tokens", topic, len(desc))
		}
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
	if raw, err := Skills.ReadFile(SkillsDir + "/lease/SKILL.md"); err != nil || !strings.Contains(string(raw), "--park") {
		t.Errorf("the lease skill names no --park: %v", err)
	}
}
