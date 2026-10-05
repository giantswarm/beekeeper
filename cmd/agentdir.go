package cmd

import (
	"os"
	"path/filepath"
	"strings"
)

// agentDir is the folder an agent beekeeper starts runs in, dir as given
// (empty: agents.dir, else the caller's). With agents.dir set it must be
// agents.dir or under one of agents.roots: a session runs in its folder for
// good, and one started anywhere else loads none of the desk's project
// instructions.
func (a *app) agentDir(dir string) (string, error) {
	home := a.cfg.Agents.Dir
	if dir == "" {
		dir = home
	}
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(homePath(dir))
	if err != nil {
		return "", err
	}
	if home == "" {
		return abs, nil
	}
	if abs == filepath.Clean(homePath(home)) {
		return abs, nil
	}
	var roots []string
	for _, r := range a.cfg.Agents.Roots {
		if inDir(abs, homePath(r)) {
			return abs, nil
		}
		roots = append(roots, homePath(r))
	}
	return "", usageErr("--dir %s: agents run in %s (agents.dir) or a worktree under agents.roots (%s), never elsewhere",
		abs, homePath(home), strings.Join(roots, ", "))
}

// homePath expands a leading ~/ to the home folder.
func homePath(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, rest)
		}
	}
	return p
}

// inDir reports whether path is dir or below it.
func inDir(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}
