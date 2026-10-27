package guard

import (
	"os"
	"path/filepath"
	"strings"
)

// Under reports whether any of paths (absolute; empty ones skipped) is one
// of dirs or lies below one, or is inside a git worktree whose main
// working tree lies below one: the directory part of the hooks' scope.
func Under(dirs []string, paths ...string) bool {
	for _, p := range paths {
		if p == "" {
			continue
		}
		p = filepath.Clean(p)
		if underOne(dirs, p) {
			return true
		}
		if mainRepo := worktreeMainRepo(p); mainRepo != "" && underOne(dirs, mainRepo) {
			return true
		}
	}
	return false
}

func underOne(dirs []string, p string) bool {
	realP, _ := filepath.EvalSymlinks(p)
	for _, d := range dirs {
		if hasDirPrefix(p, d) || (realP != "" && hasDirPrefix(realP, d)) {
			return true
		}
		if realD, err := filepath.EvalSymlinks(d); err == nil && realD != "" {
			if hasDirPrefix(p, realD) || (realP != "" && hasDirPrefix(realP, realD)) {
				return true
			}
		}
	}
	return false
}

func hasDirPrefix(p, d string) bool {
	return p == d || strings.HasPrefix(p, strings.TrimSuffix(d, string(filepath.Separator))+string(filepath.Separator))
}

// worktreeMainRepo returns the main repository root when dir lies inside a
// linked git worktree (whose .git is a gitdir: file pointing into
// <main>/.git/worktrees/<name>), or "" otherwise.
func worktreeMainRepo(dir string) string {
	for cur := filepath.Clean(dir); ; {
		dotGit := filepath.Join(cur, ".git")
		if fi, err := os.Lstat(dotGit); err == nil && fi.Mode().IsRegular() {
			raw, err := os.ReadFile(dotGit) //nolint:gosec // reading .git pointer file to resolve worktree root
			if err != nil {
				return ""
			}
			line := strings.TrimSpace(string(raw))
			gitdir, ok := strings.CutPrefix(line, "gitdir:")
			if !ok {
				return ""
			}
			gitdir = strings.TrimSpace(gitdir)
			if !filepath.IsAbs(gitdir) {
				gitdir = filepath.Join(cur, gitdir)
			}
			gitdir = filepath.Clean(gitdir)
			if commondirRaw, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil { //nolint:gosec // git worktree commondir pointer
				cd := strings.TrimSpace(string(commondirRaw))
				if !filepath.IsAbs(cd) {
					cd = filepath.Join(gitdir, cd)
				}
				cd = filepath.Clean(cd)
				if filepath.Base(cd) == ".git" {
					return filepath.Dir(cd)
				}
			}
			if filepath.Base(filepath.Dir(gitdir)) == "worktrees" && filepath.Base(filepath.Dir(filepath.Dir(gitdir))) == ".git" {
				return filepath.Dir(filepath.Dir(filepath.Dir(gitdir)))
			}
			return ""
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}
