package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

const diskPathTop = 3
const diskPathDepth = 6
const diskPathEntries = 50000
const diskPathBudget = 2 * time.Second
const diskPathFileMin = 16 << 20

// diskPathOtherFiles stands for a root's smaller files in their shared row.
const diskPathOtherFiles = "(other files)"

type diskPath struct {
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
	Growth  *int64 `json:"growthBytes,omitempty"`
	Partial bool   `json:"partial,omitempty"`
}

// Only metadata is read. Symlinks are never followed. Depth, entries and
// deadline bound work; partial totals are lower bounds and never yield growth.
// Each root has its own entries and is read at the same time as the others,
// and each directory of a root gets a fair share of what is left: a large
// state directory must not leave the user cache, or the rest of itself,
// unread. What stays unread shows: a directory found but not measured is a
// partial row, and a root not read to its end is a partial row of its own.
// A file of a root at diskPathFileMin or more has a row, its smaller files
// share one. A directory in skip is measured elsewhere.
func scanDiskPaths(ctx context.Context, roots []string, depth, limit int, skip ...string) []diskPath {
	skipped := make(map[string]bool, len(skip))
	for _, p := range skip {
		skipped[p] = true
	}
	found := make([][]diskPath, len(roots))
	var wg sync.WaitGroup
	for i, root := range roots {
		wg.Go(func() { found[i] = scanDiskRoot(ctx, root, depth, limit, skipped) })
	}
	wg.Wait()
	return slices.Concat(found...)
}

func scanDiskRoot(ctx context.Context, root string, depth, limit int, skip map[string]bool) []diskPath {
	f, err := os.OpenInRoot(root, ".")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []diskPath{{Path: root, Partial: true}}
	}
	defer func() { _ = f.Close() }()
	var out []diskPath
	var dirs []string
	var other int64
	remaining, whole, partial := limit, false, false
	for ctx.Err() == nil && remaining > 0 && !whole {
		entries, err := f.ReadDir(min(128, remaining))
		remaining -= len(entries)
		for _, e := range entries {
			path := filepath.Join(root, e.Name())
			switch {
			case e.Type()&os.ModeSymlink != 0 || skip[path]:
			case e.IsDir():
				dirs = append(dirs, path)
			case e.Type().IsRegular():
				info, err := e.Info()
				if err != nil {
					partial = true
				} else if info.Size() >= diskPathFileMin {
					out = append(out, diskPath{Path: path, Bytes: info.Size()})
				} else {
					other += info.Size()
				}
			}
		}
		if err != nil {
			whole = err == io.EOF
			break
		}
	}
	out = append(out, measureDiskDirs(ctx, dirs, depth, remaining, skip)...)
	if other > 0 || partial {
		out = append(out, diskPath{Path: filepath.Join(root, diskPathOtherFiles), Bytes: other, Partial: partial})
	}
	if !whole {
		out = append(out, diskPath{Path: root, Partial: true})
	}
	return out
}

// measureDiskDirs has a row for every one of dirs, each measured with a fair
// share of remaining; what a share leaves goes to the ones after it.
func measureDiskDirs(ctx context.Context, dirs []string, depth, remaining int, skip map[string]bool) []diskPath {
	out := make([]diskPath, 0, len(dirs))
	for i, path := range dirs {
		todo := len(dirs) - i
		share := (max(remaining, 0) + todo - 1) / todo
		left := share
		n, p := diskPathSize(ctx, path, 1, depth, &left, skip)
		remaining -= share - left
		out = append(out, diskPath{Path: path, Bytes: n, Partial: p})
	}
	return out
}

// diskPathSize adds up the regular files under path, spending remaining on
// every entry it reads.
func diskPathSize(ctx context.Context, path string, level, depth int, remaining *int, skip map[string]bool) (int64, bool) {
	if ctx.Err() != nil || *remaining <= 0 {
		return 0, true
	}
	*remaining--
	f, err := os.OpenInRoot(path, ".")
	if err != nil {
		return 0, true
	}
	defer func() { _ = f.Close() }()
	var total int64
	partial := false
	for {
		if ctx.Err() != nil || *remaining <= 0 {
			return total, true
		}
		entries, err := f.ReadDir(128)
		for _, e := range entries {
			if ctx.Err() != nil || *remaining <= 0 {
				return total, true
			}
			*remaining--
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			if e.IsDir() {
				child := filepath.Join(path, e.Name())
				if skip[child] {
					continue
				}
				if level >= depth {
					partial = true
					continue
				}
				n, p := diskPathSize(ctx, child, level+1, depth, remaining, skip)
				total += n
				partial = partial || p
			} else if e.Type().IsRegular() {
				info, err := e.Info()
				if err != nil {
					partial = true
					continue
				}
				total += info.Size()
			}
		}
		if err == io.EOF {
			return total, partial
		}
		if err != nil {
			return total, true
		}
	}
}

// diskHarnessRoots are the harness worktree directories of the checkouts.
func (a *app) diskHarnessRoots() []string {
	var roots []string
	for _, checkout := range append(slices.Clone(a.cfg.Disk.Checkouts), filepath.Join(a.cfg.Projects.Dir, "*")) {
		if nested, err := filepath.Glob(filepath.Join(checkout, ".claude", "worktrees")); err == nil {
			roots = append(roots, nested...)
		}
	}
	return roots
}

// diskPathRoots are the places read beyond the home's state and cache:
// beekeeper's state by its subdirectories, the worktree places, and the
// harness worktrees of the checkouts.
func (a *app) diskPathRoots() []string {
	roots := append([]string{a.cfg.StateDir}, a.cfg.Projects.Worktrees...)
	roots = append(roots, a.diskHarnessRoots()...)
	slices.Sort(roots)
	return slices.Compact(slices.DeleteFunc(roots, func(p string) bool { return p == "" }))
}

func readDiskPaths(ctx context.Context, extra []string) []diskPath {
	ctx, cancel := context.WithTimeout(ctx, diskPathBudget)
	defer cancel()
	home, _ := os.UserHomeDir()
	roots := []string{filepath.Join(home, ".local/state"), filepath.Join(home, ".cache")}
	// Ask Docker for its configured root, including rootless installations.
	dockerCtx, stop := context.WithTimeout(ctx, 300*time.Millisecond)
	if exe, err := proc.LookTool("docker"); err == nil {
		if b, err := proc.Command(dockerCtx, exe, "info", "--format", "{{.DockerRootDir}}").Output(); err == nil {
			if root := strings.TrimSpace(string(b)); filepath.IsAbs(root) {
				roots = append(roots, root)
			}
		}
	}
	stop()
	if len(roots) == 2 {
		roots = append(roots, "/var/lib/docker")
	}
	return scanDiskPaths(ctx, append(roots, extra...), diskPathDepth, diskPathEntries, extra...)
}

func diskPathGrowth(cur, prev []diskPath) {
	old := map[string]diskPath{}
	for _, p := range prev {
		old[p.Path] = p
	}
	for i := range cur {
		cur[i].Growth = nil
		if p, ok := old[cur[i].Path]; ok && !p.Partial && !cur[i].Partial {
			d := cur[i].Bytes - p.Bytes
			cur[i].Growth = &d
		}
	}
}

func topDiskPaths(paths []diskPath, growth bool, n int) []diskPath {
	out := slices.Clone(paths)
	if growth {
		out = slices.DeleteFunc(out, func(p diskPath) bool { return p.Growth == nil || *p.Growth <= 0 })
	}
	value := func(p diskPath) int64 {
		if growth {
			return *p.Growth
		}
		return p.Bytes
	}
	slices.SortFunc(out, func(a, b diskPath) int {
		if value(a) > value(b) {
			return -1
		}
		if value(a) < value(b) {
			return 1
		}
		return strings.Compare(a.Path, b.Path)
	})
	return out[:min(n, len(out))]
}

func diskPathText(p diskPath) string {
	mark := ""
	if p.Partial {
		mark = " (partial lower bound)"
	}
	growth := "; growth unknown"
	if p.Growth != nil {
		growth = fmt.Sprintf("; growth %+.3f GiB", float64(*p.Growth)/(1<<30))
	}
	return fmt.Sprintf("%q %.3f GiB%s%s", p.Path, float64(p.Bytes)/(1<<30), mark, growth)
}

func diskPathLines(paths []diskPath) []string {
	var lines []string
	for _, group := range []struct {
		name   string
		growth bool
	}{{"size", false}, {"growth", true}} {
		for _, p := range topDiskPaths(paths, group.growth, diskPathTop) {
			lines = append(lines, "disk paths by "+group.name+": "+diskPathText(p))
		}
	}
	return lines
}
