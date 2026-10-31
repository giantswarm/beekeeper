package guard

import (
	"fmt"
	"path/filepath"
)

// ProjectInstructions supplies Claude project files to a harness that does not
// load them itself. AGENTS.md alone stays with the harness's native loader.
// Discovery uses the same import, secret and repository boundaries as MandatoryReads.
// A directory outside Git is its own instruction boundary.
func ProjectInstructions(dir string, budget int) string {
	if budget <= 0 {
		return ""
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	repo := RepoRoot(dir)
	if repo == "" {
		repo = dir
	}
	r := reader{repo: repo, seen: map[string]bool{}}
	var dirs []string
	for d := dir; ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if d == repo {
			break
		}
	}
	claudeFiles := false
	for i := len(dirs) - 1; i >= 0; i-- {
		claudeFiles = r.gather(dirs[i]) || claudeFiles
	}
	if !claudeFiles {
		return ""
	}
	intro := fmt.Sprintf("beekeeper: project instructions for %s, composed before this session starts. They apply for the whole task. "+
		"AGENTS.md takes precedence over conflicting CLAUDE.md instructions, whether tracked by Git or not; .claude/rules also apply. "+
		"Instructions nearer the working directory take precedence over their ancestors. Read any files named as omitted or truncated before working.\n", dir)
	return r.renderWithIntro(budget, intro)
}
