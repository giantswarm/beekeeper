// Package plugin carries the Claude Code plugin beekeeper ships, the role
// skills, for the commands that hand them to sessions themselves.
package plugin

import (
	"embed"
	"io/fs"
	"strings"
)

// Skills is the plugin's skills folder, one <name>/SKILL.md each.
//
//go:embed skills
var Skills embed.FS

// SkillsDir is Skills' root folder.
const SkillsDir = "skills"

// Topics are the worker rules' topic skills, in the order the rules name
// them: the detail a worker loads before the step that needs it.
var Topics = []string{"lease", "merge", "github", "park"}

// SkillNames are the plugin's skills, by name, sorted.
func SkillNames() []string {
	entries, err := fs.ReadDir(Skills, SkillsDir)
	if err != nil {
		panic(err) // embedded at build time
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// SkillFile is the named skill's SKILL.md as embedded, frontmatter and all.
func SkillFile(name string) []byte {
	raw, err := Skills.ReadFile(SkillsDir + "/" + name + "/SKILL.md")
	if err != nil {
		panic(err) // embedded at build time
	}
	return raw
}

// Skill is the named skill's text without its frontmatter.
func Skill(name string) string {
	return strings.TrimSpace(body(string(SkillFile(name))))
}

// WorkerRules is the worker-rules skill's text without its frontmatter: the
// core rules every worker on the desk follows, which name the topics.
func WorkerRules() string {
	return Skill("worker-rules")
}

// body is a Markdown file's text after its YAML frontmatter, if any.
func body(s string) string {
	rest, ok := strings.CutPrefix(s, "---\n")
	if !ok {
		return s
	}
	if _, after, ok := strings.Cut(rest, "\n---\n"); ok {
		return after
	}
	return s
}
