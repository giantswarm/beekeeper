// Package plugin carries the Claude Code plugin beekeeper ships, the role
// skills, for the commands that hand them to sessions themselves.
package plugin

import (
	"embed"
	"strings"
)

// Skills is the plugin's skills folder, one <name>/SKILL.md each.
//
//go:embed skills
var Skills embed.FS

// SkillsDir is Skills' root folder.
const SkillsDir = "skills"

// WorkerRules is the worker-rules skill's text without its frontmatter: the
// rules every worker on the desk follows.
func WorkerRules() string {
	raw, err := Skills.ReadFile(SkillsDir + "/worker-rules/SKILL.md")
	if err != nil {
		panic(err) // embedded at build time
	}
	return strings.TrimSpace(body(string(raw)))
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
