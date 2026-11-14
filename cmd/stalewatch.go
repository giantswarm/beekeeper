package cmd

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// rearm says how the watch running args is re-armed on the installed
// binary: stopped, then started again with its own command line, which its
// owner completes when display left a value out of it.
func rearm(args []string) string {
	s, cut := masked(args)
	if cut {
		return "stop it, then run `" + s + "` with its values"
	}
	return "stop it, then run `" + s + "`"
}

// isWatch reports whether p is a long-running beekeeper watch (beekeeper
// watch, alerts watch, guide watch); a --once poll ends by itself.
func isWatch(p *proc.Process) bool {
	return p.Comm == project.Name && slices.Contains(p.Args, "watch") && !slices.Contains(p.Args, "--once")
}

// staleBinary is a beekeeper process running a binary an install replaced,
// and that install.
type staleBinary struct {
	*proc.Process
	by platform.Install
}

// staleBinaries are the beekeeper processes of t, self apart, whose binary
// another file was renamed over since they started (replacement says which
// for a pid): an install's leftovers, which run the old code and whose saves
// of the state the new release refuses. An install that arrived before a
// process started never replaced the binary it runs, whatever file it runs:
// a copy elsewhere on the PATH, a file another mount namespace holds at the
// path. The recorded start can be up to a second early because /proc/stat
// rounds boot time down. A replacement inside that uncertain second still
// counts: only an install at or before the recorded start is proven earlier.
// By pid.
func staleBinaries(t *proc.Table, self int, replacement func(pid int) (platform.Install, bool)) []staleBinary {
	var out []staleBinary
	for _, p := range t.ByPID {
		if p.PID == self || p.Comm != project.Name {
			continue
		}
		in, ok := replacement(p.PID)
		if !ok || !in.At.IsZero() && !p.Start.IsZero() && !in.At.After(p.Start) {
			continue
		}
		out = append(out, staleBinary{Process: p, by: in})
	}
	slices.SortFunc(out, func(a, b staleBinary) int { return a.PID - b.PID })
	return out
}

// isStaleWatch is isWatch for a stale binary.
func isStaleWatch(s staleBinary) bool { return isWatch(s.Process) }

// replacedBinary is the install renamed over the executable of the live
// process pid since it started, false for none.
func replacedBinary(pid int) (platform.Install, bool) {
	return platform.ProcessBinary(pid).Replacement()
}

// binaryVersion is the version the beekeeper binary file reports, "an
// unknown version" when it does not run.
func binaryVersion(ctx context.Context, file string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := proc.Command(ctx, file, "version").Output()
	// "beekeeper v0.63.0 (commit …, built …)"
	if f := strings.Fields(string(out)); err == nil && len(f) > 1 && f[0] == project.Name {
		return f[1]
	}
	return "an unknown version"
}
