package cmd

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/config"
)

// diskUsageEntries bounds what one disk usage reads; its deadline is --budget.
const diskUsageEntries = 5_000_000

// diskUsageDepth is deeper than the watch's: a person asked for the sizes.
const diskUsageDepth = 32

const diskOwnerNoPolicy = "no disk.caches policy"

// diskUsageRow is one directory of the disk view with who answers for it.
type diskUsageRow struct {
	diskPath
	Owner string `json:"owner"`
}

// diskUsage reads beekeeper's state by its subdirectories, the user cache
// directory by its programs and the worktree places. Only metadata is read.
func (a *app) diskUsage(ctx context.Context, budget time.Duration) []diskUsageRow {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	cache, _ := os.UserCacheDir()
	roots := a.diskPathRoots()
	if cache != "" {
		roots = append(roots, cache)
	}
	entries := a.diskCacheEntries()
	var rows []diskUsageRow
	for _, p := range scanDiskPaths(ctx, roots, diskUsageDepth, diskUsageEntries) {
		rows = append(rows, diskUsageRow{p, a.diskOwner(p.Path, cache, entries)})
	}
	slices.SortFunc(rows, func(x, y diskUsageRow) int {
		return cmp.Or(cmp.Compare(y.Bytes, x.Bytes), cmp.Compare(x.Path, y.Path))
	})
	return rows
}

// diskOwner says who answers for one directory of the disk view. A cache
// directory goes by its program, which the write counters cannot tell.
func (a *app) diskOwner(path, cache string, entries []config.DiskCache) string {
	switch {
	case filepath.Dir(path) == a.cfg.StateDir:
		return pluginName
	case cache != "" && filepath.Dir(path) == cache:
		if name, ok := cachePolicy(path, entries); ok {
			return "cache " + name
		}
		return filepath.Base(path) + " (" + diskOwnerNoPolicy + ")"
	case strings.Contains(filepath.ToSlash(path), "/.claude/worktrees/"):
		return "harness worktree"
	}
	return "worktree"
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
covers it.

Only directory metadata is read, never a file's content, and nothing is
removed. A row marked "at least" was cut short by --budget: raise it.`,
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
