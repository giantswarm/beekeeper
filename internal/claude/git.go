package claude

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// GitInfo returns the GitHub repository ("owner/repo" from the origin remote)
// and the checked-out branch of the checkout dir lies in, reading .git
// directly: a worktree's .git file, its commondir, HEAD and config. Empty
// strings when dir is not in a git checkout.
func GitInfo(dir string) (repo, branch string) {
	gitdir := findGitDir(dir)
	if gitdir == "" {
		return "", ""
	}
	common := gitdir
	if raw, err := os.ReadFile(filepath.Clean(filepath.Join(gitdir, "commondir"))); err == nil {
		c := strings.TrimSpace(string(raw))
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitdir, c)
		}
		common = filepath.Clean(c)
	}
	if raw, err := os.ReadFile(filepath.Clean(filepath.Join(gitdir, "HEAD"))); err == nil {
		head := strings.TrimSpace(string(raw))
		if ref, ok := strings.CutPrefix(head, "ref: refs/heads/"); ok {
			branch = ref
		} else if len(head) >= 7 {
			branch = head[:7]
		}
	}
	return originRepo(filepath.Join(common, "config")), branch
}

func findGitDir(dir string) string {
	for d := dir; d != "" && d != "/" && d != "."; d = filepath.Dir(d) {
		p := filepath.Join(d, ".git")
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.IsDir() {
			return p
		}
		raw, err := os.ReadFile(filepath.Clean(p))
		if err != nil {
			return ""
		}
		g, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir: ")
		if !ok {
			return ""
		}
		if !filepath.IsAbs(g) {
			g = filepath.Join(d, g)
		}
		return filepath.Clean(g)
	}
	return ""
}

// originRepo reads the url of [remote "origin"] from a git config file.
func originRepo(path string) string {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			in = line == `[remote "origin"]`
			continue
		}
		if !in {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == "url" {
			return RepoFromURL(strings.TrimSpace(v))
		}
	}
	return ""
}

// RepoFromURL turns a GitHub remote URL into "owner/repo"; other hosts keep
// "host/owner/repo".
func RepoFromURL(u string) string {
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	for _, p := range []string{"git@github.com:", "ssh://git@github.com/", "https://github.com/", "http://github.com/"} {
		if r, ok := strings.CutPrefix(u, p); ok {
			return r
		}
	}
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if at := strings.Index(u, "@"); at >= 0 {
		u = u[at+1:]
	}
	return strings.Replace(u, ":", "/", 1)
}
