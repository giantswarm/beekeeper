package cmd

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/worktrees"
)

// diskUsageEntries bounds what one disk usage reads; its deadline is --budget.
const diskUsageEntries = 5_000_000

// diskUsageDepth is deeper than the watch's: a person asked for the sizes.
const diskUsageDepth = 32

const (
	diskOwnerNoPolicy = "no disk.caches policy"
	diskOwnerCache    = "user cache"
	diskOwnerPlace    = "worktree place"
	diskOwnerTree     = "worktree"
	diskOwnerHarness  = "harness worktree"
)

// diskUsageRow is one directory of the disk view with who answers for it.
type diskUsageRow struct {
	diskPath
	Owner string `json:"owner"`
}

// diskWorktrees lists the worktrees in their places, whatever a branch name
// nests them in. The walk is the prune's; past ctx it is not waited for, and
// its places then show as directories.
func (a *app) diskWorktrees(ctx context.Context) []string {
	places := append(slices.Clone(a.cfg.Projects.Worktrees), a.cfg.Doctor.WorktreeRoots...)
	places = append(places, filepath.Join(a.cfg.StateDir, "worktrees"))
	places = append(places, a.diskHarnessRoots()...)
	found := make(chan []string, 1)
	go func() { found <- worktrees.Find(places) }()
	select {
	case trees := <-found:
		return trees
	case <-ctx.Done():
		return nil
	}
}

// diskUsage reads beekeeper's state by its subdirectories, the user cache
// directory by its programs and each worktree on its own; a worktree place
// shows what lies outside its worktrees. Only metadata is read.
func (a *app) diskUsage(ctx context.Context, budget time.Duration) []diskUsageRow {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	cache, _ := os.UserCacheDir()
	roots := a.diskPathRoots()
	if cache != "" {
		roots = append(roots, cache)
	}
	entries := a.diskCacheEntries()
	trees := a.diskWorktrees(ctx)
	var measured []diskPath
	var wg sync.WaitGroup
	wg.Go(func() { measured = measureDiskDirs(ctx, trees, diskUsageDepth, diskUsageEntries, nil) })
	var rows []diskUsageRow
	for _, p := range scanDiskPaths(ctx, roots, diskUsageDepth, diskUsageEntries, trees...) {
		rows = append(rows, diskUsageRow{p, a.diskOwner(p.Path, cache, entries)})
	}
	wg.Wait()
	for _, p := range measured {
		owner := diskOwnerTree
		if strings.Contains(filepath.ToSlash(p.Path), "/.claude/worktrees/") {
			owner = diskOwnerHarness
		}
		rows = append(rows, diskUsageRow{p, owner})
	}
	slices.SortFunc(rows, func(x, y diskUsageRow) int {
		return cmp.Or(cmp.Compare(y.Bytes, x.Bytes), cmp.Compare(x.Path, y.Path))
	})
	return rows
}

// diskOwner says who answers for one row of the disk view. A cache directory
// goes by its program, which the write counters cannot tell.
func (a *app) diskOwner(path, cache string, entries []config.DiskCache) string {
	switch {
	case path == a.cfg.StateDir || filepath.Dir(path) == a.cfg.StateDir:
		return pluginName
	case cache != "" && (path == cache || path == filepath.Join(cache, diskPathOtherFiles)):
		return diskOwnerCache
	case cache != "" && filepath.Dir(path) == cache:
		if name, ok := cachePolicy(path, entries); ok {
			return "cache " + name
		}
		return filepath.Base(path) + " (" + diskOwnerNoPolicy + ")"
	}
	return diskOwnerPlace
}

func (a *app) diskUsageCmd() *cobra.Command {
	var minMiB int
	var budget time.Duration
	c := &cobra.Command{
		Use:   "usage",
		Short: "List what holds the disk by owner: beekeeper's state, the user cache and the worktrees; remove nothing",
		Long: `List the size of each subdirectory of beekeeper's state, of each program's
directory in the user cache, and of each worktree (projects.worktrees and a
checkout's harness worktrees), largest first, with who answers for it. A
cache directory names its disk.caches policy, or its program when no policy
covers it. A file directly in one of these places has its own row from 16 MiB,
such as beekeeper's audit log; the smaller ones share "(other files)". A
worktree place's row is what lies outside its worktrees.

Only directory metadata is read, never a file's content, and nothing is
removed. A row marked "at least" or "unread" was cut short by --budget or the
entry limit, and one naming the place itself says the place was not read to
its end: raise --budget.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rows := a.diskUsage(cmd.Context(), budget)
			if a.json {
				return a.printJSON(rows)
			}
			w := a.table()
			_, _ = fmt.Fprintln(w, "  SIZE\tPATH\tOWNER")
			var hidden int
			var hiddenBytes int64
			for _, r := range rows {
				if r.Bytes < int64(minMiB)<<20 && !r.Partial {
					hidden++
					hiddenBytes += r.Bytes
					continue
				}
				size := sizeStr(r.Bytes)
				if r.Partial {
					size = "at least " + size
					if r.Bytes == 0 {
						size = "unread"
					}
				}
				_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\n", size, r.Path, r.Owner)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if hidden > 0 {
				_, err := fmt.Fprintf(a.out, "  and %d directories under %d MiB, %s (--min-mib 0 lists them)\n", hidden, minMiB, sizeStr(hiddenBytes))
				return err
			}
			return nil
		},
	}
	c.Flags().IntVar(&minMiB, "min-mib", 100, "list only directories of at least this many MiB")
	c.Flags().DurationVar(&budget, "budget", time.Minute, "stop reading after this long and mark what stayed unread")
	return c
}
