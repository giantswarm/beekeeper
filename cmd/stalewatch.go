package cmd

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// staleKey starts the condition key of a watch whose binary was replaced.
const staleKey = "stale-binary "

// staleWatches says each running beekeeper watch, this one included, whose
// binary self-update replaced: it still runs the old code, which only a
// re-arm (a restart) leaves. One WATCH STALE line per watch names the
// version it runs and the one installed, and one ENDED line follows once it
// exits. The watch never re-executes itself: its owner re-arms it.
func (w *watcher) staleWatches(ctx context.Context, t *proc.Table) {
	var owners map[int]string
	if o := w.owners.Load(); o != nil {
		owners = *o
	}
	found := map[string]bool{}
	for _, p := range t.ByPID {
		if !isWatch(p) {
			continue
		}
		path, ok := w.replacedBinary(p.PID)
		if !ok {
			continue
		}
		key := fmt.Sprintf("%s%d", staleKey, p.PID)
		found[key] = true
		if !w.isActive(key) {
			w.emit(key, "%s", w.staleLine(ctx, p, path, owner(t, p.PID, owners)))
		}
	}
	w.clearMissing(staleKey, found)
}

// staleLine is a stale watch's line: which watch, the version it runs, the
// one installed at its path.
func (w *watcher) staleLine(ctx context.Context, p *proc.Process, path, session string) string {
	who, running := fmt.Sprintf("%s (pid %d", display(p.Args), p.PID), project.Version()
	if p.PID == os.Getpid() {
		who = "this watch (pid " + fmt.Sprint(p.PID)
	} else {
		running = w.binaryVersion(ctx, fmt.Sprintf("/proc/%d/exe", p.PID))
	}
	if session != "" {
		who += ", session " + session
	}
	return fmt.Sprintf("WATCH STALE %s): runs %s; self-update installed %s at %s, a re-arm picks it up",
		who, running, w.binaryVersion(ctx, path), path)
}

// isWatch reports whether p is a long-running beekeeper watch (beekeeper
// watch, alerts watch, guide watch); a --once poll ends by itself.
func isWatch(p *proc.Process) bool {
	return p.Comm == project.Name && slices.Contains(p.Args, "watch") && !slices.Contains(p.Args, "--once")
}

// staleBinaries are the beekeeper processes of t, self apart, whose binary
// another file was renamed over since they started (replaced says so for a
// pid): an install's leftovers, which run the old code and whose saves of
// the state the new release refuses. By pid.
func staleBinaries(t *proc.Table, self int, replaced func(pid int) bool) []*proc.Process {
	var out []*proc.Process
	for _, p := range t.ByPID {
		if p.PID != self && p.Comm == project.Name && replaced(p.PID) {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b *proc.Process) int { return a.PID - b.PID })
	return out
}

// replacedBinary says whether another file was renamed over the executable
// of the live process pid since it started.
func replacedBinary(pid int) bool { return platform.ProcessBinary(pid).Replaced() }

// replacedBinary is the path of process pid's executable and whether
// another file was renamed over it since the process started.
func (w *watcher) replacedBinary(pid int) (string, bool) {
	if w.replaced != nil {
		return w.replaced(pid)
	}
	b := platform.ProcessBinary(pid)
	if b == nil {
		return "", false
	}
	return b.Path, b.Replaced()
}

// binaryVersion is the version the beekeeper binary file reports, "an
// unknown version" when it does not run.
func (w *watcher) binaryVersion(ctx context.Context, file string) string {
	if w.versionOf != nil {
		return w.versionOf(ctx, file)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := proc.Command(ctx, file, "version").Output()
	// "beekeeper v0.63.0 (commit …, built …)"
	if f := strings.Fields(string(out)); err == nil && len(f) > 1 && f[0] == project.Name {
		return f[1]
	}
	return "an unknown version"
}
