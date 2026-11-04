package plugin

import (
	"os"
	"os/exec"
	"regexp"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestEmbeddedSkillsReleaseConvention(t *testing.T) {
	for _, tc := range []struct {
		name  string
		title string
		paths []string
		want  bool
	}{
		{"rules docs rejected", "docs(rules): update rules", []string{"plugin/skills/worker-rules/SKILL.md"}, false},
		{"topic docs rejected", "docs: update topic", []string{"plugin/skills/merge/SKILL.md"}, false},
		{"nested skill file rejected", "test: update fixture", []string{"plugin/skills/example/references/example.md"}, false},
		{"rules fix releases", "fix(rules): update rules", []string{"plugin/skills/worker-rules/SKILL.md"}, true},
		{"skill feature releases", "feat(skills): add behavior", []string{"plugin/skills/new/SKILL.md"}, true},
		{"breaking rules fix releases", "fix(rules)!: change contract", []string{"plugin/skills/worker-rules/SKILL.md"}, true},
		{"ordinary docs allowed", "docs: update readme", []string{"README.md", "docs/usage.md"}, true},
		{"mixed docs rejected", "docs: update guidance", []string{"README.md", "plugin/skills/worker-rules/SKILL.md"}, false},
		{"no changed files", "docs: update readme", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"../.github/check-embedded-skills-release.sh", tc.title}, tc.paths...)
			output, err := exec.Command("bash", args...).CombinedOutput()
			if (err == nil) != tc.want {
				t.Fatalf("allowed = %v, want %v: %s", err == nil, tc.want, output)
			}
		})
	}
}

// Keep the convention compatible with devctl's generated release sections:
// accepted embedded-skill subjects must release; ordinary docs must not.
func TestEmbeddedSkillsReleaseSections(t *testing.T) {
	raw, err := os.ReadFile("../cliff.toml")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Git struct {
			CommitParsers []struct {
				Message string
				Group   string
				Skip    bool
			} `toml:"commit_parsers"`
		} `toml:"git"`
	}
	if err := toml.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"feat", "fix", "refactor", "perf", "chore", "build", "ci", "security", "docs", "style", "test"} {
		t.Run(kind, func(t *testing.T) {
			subject := kind + "(rules): update guidance"
			var releases bool
			for _, parser := range config.Git.CommitParsers {
				if regexp.MustCompile(parser.Message).MatchString(subject) {
					releases = !parser.Skip && parser.Group != ""
					break
				}
			}
			output, err := exec.Command("bash", "../.github/check-embedded-skills-release.sh", subject, "plugin/skills/worker-rules/SKILL.md").CombinedOutput()
			if (err == nil) != releases {
				t.Fatalf("convention allows = %v, release section = %v: %s", err == nil, releases, output)
			}
			if kind == "docs" && releases {
				t.Fatal("ordinary docs must remain non-releasing")
			}
		})
	}
}
