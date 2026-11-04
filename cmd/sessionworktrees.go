package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/worktrees"
)

func (a *app) sessionWorktreeHook(remove bool) *cobra.Command {
	name := "worktreecreate"
	if remove {
		name = "worktreeremove"
	}
	return &cobra.Command{Use: name, Short: "Place Claude session worktrees outside checkouts", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			raw, err := io.ReadAll(c.InOrStdin())
			if err != nil {
				return err
			}
			var ev struct {
				Cwd  string `json:"cwd"`
				Name string `json:"name"`
				Path string `json:"worktree_path"`
			}
			if err := json.Unmarshal(raw, &ev); err != nil {
				return err
			}
			if err := a.loadConfig(); err != nil {
				return err
			}
			ctx := context.Background()
			if remove {
				// Never force removal or delete branches: dirty and unpushed work stays recoverable.
				root, err := filepath.EvalSymlinks(a.cfg.Projects.Worktrees)
				if err != nil {
					return err
				}
				path, err := filepath.EvalSymlinks(ev.Path)
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(root, path)
				if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || !worktrees.Linked(ev.Path) {
					return fmt.Errorf("worktree is outside projects.worktrees")
				}
				clean, err := worktrees.SessionRemovable(ctx, runGit, ev.Path)
				if err != nil {
					return err
				}
				if !clean {
					return fmt.Errorf("worktree has unmerged or dirty work; retained")
				}
				_, err = runGit(ctx, ev.Path, "worktree", "remove", "--", ev.Path)
				return err
			}
			repo, err := worktrees.MainCheckout(ctx, runGit, ev.Cwd)
			if err != nil {
				return err
			}
			branch := "worktree-" + ev.Name
			if ev.Name == "" {
				return fmt.Errorf("worktree name is required")
			}
			if _, err := runGit(ctx, repo, "check-ref-format", "--branch", branch); err != nil {
				return err
			}
			path, err := worktrees.SessionPath(a.cfg.Projects.Worktrees, repo, branch)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
				return err
			}
			if _, err := runGit(ctx, ev.Cwd, "worktree", "add", "-b", branch, "--", path, "HEAD"); err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, path)
			return err
		}}
}
