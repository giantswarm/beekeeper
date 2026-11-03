// Package runrecord formats and reads the run.start / run.end / run.fallback
// events every beekeeper run writes to the event log, local and remote
// alike: one record shape, so `beekeeper log --verb run.` and
// `beekeeper run stats` count builds, their minutes and their failures
// whichever slot they ran in. The formatter is a leaf package:
// internal/guard and internal/remotebuild both use it, and guard imports
// remotebuild, so neither may hold the shared shape.
//
// The detail of a start is "<id><facts>: <command>", of an end
// "<id> exit <code> after <duration>[, <extra>]: <command>". The id says
// where the run ran: a local one names its scope's unit exactly as the
// kernel prints it in an OOM kill's memcg path (memcap-…, memcap-test-…), a
// remote one remote:<pod>.<run>. The facts of a start name what was built
// (the repository and HEAD) and, remotely, the builder pod's namespace and
// cache; the extra of an end names the cap's victims of a local run or the
// failure's tail of a remote one. A remote attempt that gives up before the
// command starts records a fallback with its reason; the local slot's own
// start and end then follow under the local id.
package runrecord

import (
	"fmt"
	"strings"
	"time"
)

// The event log verbs of a run.
const (
	Start    = "run.start"
	End      = "run.end"
	Fallback = "run.fallback"
)

// RemotePrefix starts the id of a run that ran in a builder pod.
const RemotePrefix = "remote:"

// RemoteID is the id a remote run's events name: remote:<pod>.<run>. It
// carries no ": " (the command follows the detail's first one) and no space
// (the id is the detail's first field).
func RemoteID(pod, run string) string { return RemotePrefix + pod + "." + short(run) }

// Facts is the repository and HEAD a run.start names — what was built —
// "" where either is unknown.
func Facts(repo, head string) string {
	if repo == "" || head == "" {
		return ""
	}
	return " " + repo + "@" + head[:min(8, len(head))]
}

// Cache is the builder pod's cache state a remote run.start names: warm for
// a reused pod whose build caches hold earlier runs, cold for one created
// for this run, with the pod's namespace. The shared cache (issue #1782)
// reports its hits here.
func Cache(warm bool, namespace string) string {
	state := " cold"
	if warm {
		state = " warm"
	}
	if namespace == "" {
		return " cache" + state
	}
	return " ns " + namespace + " cache" + state
}

// Warm is the reader of Cache: whether a run.start's facts — read before the
// command delimiter, never from the command or a failed run's tail, which
// hold arbitrary text — say the builder pod's build caches held earlier
// runs. ok is false when the record names no cache: a local run's, an end's,
// or a record of an older release.
func Warm(detail string) (warm, ok bool) {
	facts, _, _ := strings.Cut(detail, ": ")
	switch {
	case strings.Contains(facts, "cache warm"):
		return true, true
	case strings.Contains(facts, "cache cold"):
		return false, true
	}
	return false, false
}

// StartDetail is a run.start's detail: "<id><facts>: <command>".
func StartDetail(id, facts, command string) string {
	return strings.TrimRight(id+facts, " ") + ": " + command
}

// EndDetail is a run.end's detail: "<id> exit <code> after <duration>[,
// <extra>]: <command>". extra names the cap's victims of a local run, or a
// remote one's failure tail.
func EndDetail(id string, rc int, took time.Duration, extra, command string) string {
	end := fmt.Sprintf("%s exit %d after %s", id, rc, took.Round(time.Second))
	if extra != "" {
		end += ", " + extra
	}
	return end + ": " + command
}

// FallbackDetail is a run.fallback's detail: a remote attempt that gave up
// before the command started, naming why; the local slot ran the command.
func FallbackDetail(facts, reason, command string) string {
	return "local" + facts + " fell back (" + strings.ReplaceAll(reason, ": ", "; ") + "): " + command
}

// ID is the detail's first field: where the run ran.
func ID(detail string) string {
	id, _, _ := strings.Cut(detail, " ")
	return id
}

// Remote reports whether the detail is a builder pod run's.
func Remote(detail string) bool { return strings.HasPrefix(ID(detail), RemotePrefix) }

// Repository is the owner/repo the detail's first fact names — every writer
// puts it right after the id — "" where none is there (a record of an older
// release, or a run outside a GitHub worktree). Only the first fact is read:
// a failed remote run's tail is output, and its text is no repository.
func Repository(detail string) string {
	head, _, _ := strings.Cut(detail, ": ")
	f := strings.Fields(head)
	if len(f) < 2 {
		return ""
	}
	repo, sha, ok := strings.Cut(f[1], "@")
	if !ok || sha == "" || strings.Count(repo, "/") != 1 {
		return ""
	}
	return repo
}

// ParseEnd reads an end detail: the exit code and the duration between the
// run's start and its record. false when the detail is none (an event
// written by another verb or an older format).
func ParseEnd(detail string) (rc int, took time.Duration, ok bool) {
	f := strings.Fields(detail)
	if len(f) < 5 || f[1] != "exit" || f[3] != "after" {
		return 0, 0, false
	}
	if _, err := fmt.Sscan(f[2], &rc); err != nil {
		return 0, 0, false
	}
	d, err := time.ParseDuration(strings.Trim(f[4], ",:"))
	if err != nil {
		return 0, 0, false
	}
	return rc, d, true
}

// OneLine folds a multi-line text (a failed run's tail) onto one line: the
// event log prints one event per line.
func OneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
