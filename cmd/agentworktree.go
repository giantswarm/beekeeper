package cmd

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
)

var taskRepositoryReference = regexp.MustCompile(`(?:^|[\s(])([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#[0-9]+`)

var taskRepository = regexp.MustCompile(`https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)(?:/(issues|pull)/(\d+))?`)

// checkoutDir deliberately rejects linked worktrees and subdirectories: the
// harness must load the repository's own instructions from its main checkout.
func checkoutDir(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && info.IsDir()
}

func (a *app) startCheckout(dir string) (string, error) {
	dir, err := a.agentDir(dir, "")
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", usageErr("--dir %s: %v", dir, err)
	}
	dir = resolved
	if checkoutDir(dir) {
		return dir, nil
	}
	var accepted []string
	roots := append([]string{a.cfg.Agents.Dir, dir}, a.cfg.Agents.Roots...)
	for _, root := range roots {
		if root == "" {
			continue
		}
		_ = filepath.WalkDir(homePath(root), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return filepath.SkipDir
			}
			if !d.IsDir() {
				return nil
			}
			if checkoutDir(p) {
				accepted = append(accepted, p)
				return filepath.SkipDir
			}
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		})
	}
	slices.Sort(accepted)
	accepted = slices.Compact(accepted)
	return "", usageErr("--dir must be exactly one repository checkout (not a parent folder or worktree); accepted checkouts: %s", strings.Join(accepted, ", "))
}

func singleTaskRepository(brief string) error {
	repos := map[string]bool{}
	for _, m := range taskRepository.FindAllStringSubmatch(brief, -1) {
		repos[strings.ToLower(m[1])] = true
	}
	for _, m := range taskRepositoryReference.FindAllStringSubmatch(brief, -1) {
		repos[strings.ToLower(m[1])] = true
	}
	if len(repos) > 1 {
		return usageErr("task names multiple repositories; split it into one task per repository")
	}
	return nil
}

func (a *app) prepareStartWorktree(ctx context.Context, checkout, branch, brief string, review, dry bool) (string, error) {
	if err := singleTaskRepository(brief); err != nil {
		return "", err
	}
	ref := ""
	if review {
		for _, m := range taskRepository.FindAllStringSubmatch(brief, -1) {
			if m[2] == "pull" {
				// Each attempt owns its checkout. Old review rounds may still
				// have worktrees, including edits that must be preserved.
				branch = "review-" + m[3] + "-" + uuid.NewString()
				ref = "refs/pull/" + m[3] + "/head"
				break
			}
		}
		if ref == "" {
			return "", usageErr("a review start requires a pull request URL in the brief")
		}
	}
	if branch == "" {
		return "", usageErr("a worker start requires --branch for its new worktree")
	}
	if _, err := runGit(ctx, checkout, "check-ref-format", "--branch", branch); err != nil {
		return "", err
	}
	root := homePath(a.cfg.Agents.WorktreesDir)
	if root == "" {
		root = filepath.Join(a.cfg.StateDir, "worktrees")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	target := filepath.Join(root, filepath.Base(checkout), branch)
	if !inDir(target, filepath.Join(root, filepath.Base(checkout))) {
		return "", usageErr("branch escapes the worktree directory")
	}
	rules := fmt.Sprintf("Work only in the worktree %s. The main checkout %s is only the session launch directory so its repository rules load. Never commit, stash, or check out in the main checkout.\n\n", target, checkout)
	if dry {
		return rules, nil
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		return "", usageErr("worktree destination already exists or cannot be inspected: %s", target)
	}
	if ref == "" {
		// Ask the remote rather than relying on a potentially stale origin/HEAD.
		out, err := runGit(ctx, checkout, "ls-remote", "--symref", "origin", "HEAD")
		if err != nil {
			return "", err
		}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 3 && fields[0] == "ref:" && fields[2] == workerGitHubHead {
				ref = "origin/" + strings.TrimPrefix(fields[1], "refs/heads/")
				break
			}
		}
		if ref == "" {
			return "", usageErr("origin does not advertise a default branch")
		}
		defaultBranch := strings.TrimPrefix(ref, "origin/")
		if _, err := runGit(ctx, checkout, "fetch", "origin", "+refs/heads/"+defaultBranch+":refs/remotes/"+ref); err != nil {
			return "", err
		}
	} else {
		if _, err := runGit(ctx, checkout, "fetch", "origin", ref); err != nil {
			return "", err
		}
		out, err := runGit(ctx, checkout, "rev-parse", "FETCH_HEAD")
		if err != nil {
			return "", err
		}
		ref = strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return "", err
	}
	args := []string{"worktree", loopAdd, "-b", branch, target, ref}
	if review {
		args = []string{"worktree", loopAdd, "--detach", target, ref}
	}
	if _, err := runGit(ctx, checkout, args...); err != nil {
		return "", err
	}
	return rules, nil
}
