package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/free"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/worktrees"
)

func (a *app) freeNestedWorktrees(o free.Options, out io.Writer) error {
	if len(o.Only) > 0 && !slices.Contains(o.Only, free.Worktrees) {
		return nil
	}
	ctx := context.Background()
	for _, dir := range worktrees.NestedSessions(a.cfg.Projects.Dir) {
		removable, err := worktrees.SessionRemovable(ctx, runGit, dir)
		if err != nil {
			return err
		}
		group := "unmerged or dirty, to move"
		if removable {
			group = "merged and clean, removable"
		}
		if o.Summary {
			_, _ = fmt.Fprintf(out, "worktree\t%s\t%s\n", dir, group)
		} else {
			_, _ = fmt.Fprintf(out, "worktrees: %s: %s\n", group, dir)
		}
		if !o.Apply {
			continue
		}
		// The existing process/session guard also protects manual migration.
		live, err := a.sessionWorktreeLiveDirs()
		if err != nil {
			return err
		}
		for _, p := range live {
			if p == dir || strings.HasPrefix(p, dir+string(filepath.Separator)) {
				return fmt.Errorf("worktree is in use: %s", dir)
			}
		}
		if removable {
			// git itself rechecks tracked and untracked files; never force.
			_, err = runGit(ctx, dir, "worktree", "remove", "--", dir)
		} else {
			repo, e := worktrees.MainCheckout(ctx, runGit, dir)
			if e != nil {
				return e
			}
			branch, e := runGit(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
			if e != nil {
				branch = []byte(filepath.Base(dir))
			}
			if len(a.cfg.Projects.Worktrees) == 0 { return fmt.Errorf("projects.worktrees must include a session root") }
			target, e := worktrees.SessionPath(a.cfg.Projects.Worktrees[0], repo, strings.TrimSpace(string(branch)))
			if e != nil {
				return e
			}
			if e = os.MkdirAll(filepath.Dir(target), 0750); e != nil {
				return e
			}
			_, err = runGit(ctx, dir, "worktree", "move", "--", dir, target)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Migration must fail closed when either the roster or processes cannot be read.
func (a *app) sessionWorktreeLiveDirs() ([]string, error) {
	w := *a
	if w.store == nil {
		store, err := state.Open(a.cfg.StateDir)
		if err != nil {
			return nil, err
		}
		w.store = store
	}
	return w.liveFolders(time.Now())
}
