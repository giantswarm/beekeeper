package cmd

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// ungatedKey starts the condition key of a merge that runs outside the gate.
const ungatedKey = "ungated "

// gatedCommand reports whether p is a devctl command the merge gate runs: a
// pr merge, or a release promote of one repository that is no dry run (any
// other promotion runs ungated by design, the gate's own dry run among them).
func gatedCommand(p *proc.Process) bool {
	if p.Comm != merge.Tool {
		return false
	}
	if _, _, ok := merge.ParseArgs(p.Args); ok {
		return true
	}
	_, ok := merge.ParsePromote(p.Args)
	return ok
}

// isGate reports whether p is a beekeeper gate or the merge-child a gate
// runs its command in.
func isGate(p *proc.Process) bool { return p.Comm == project.Name && reexecs(p) }

// ungatedProcess reports whether p is a devctl merge or promotion that runs
// with no beekeeper gate among its ancestors: outside the lanes, the holds
// and the budget floor, with devctl's own bypass. The gate runs its command
// in a merge-child under a transient unit, so the merge-child is the
// ancestor a gated one has. A test run's fixture is none: it or one of its
// ancestors is a test process.
func ungatedProcess(t *proc.Table, p *proc.Process) bool {
	if !gatedCommand(p) || isTest(p) {
		return false
	}
	ancestors := t.Ancestors(p.PID)
	return !slices.ContainsFunc(ancestors, isGate) && !slices.ContainsFunc(ancestors, isTest)
}

// isTest reports whether p belongs to a Go test run: a test binary (cmd.test,
// any *.test) or an executable under a test's temporary directory
// (/tmp/Test…), the fake devctl a test of the gate runs. The executable is
// the first argument, or the second for a script its interpreter runs
// (sh /tmp/Test…/devctl).
func isTest(p *proc.Process) bool {
	if strings.HasSuffix(p.Comm, ".test") {
		return true
	}
	return slices.ContainsFunc(p.Args[:min(len(p.Args), 2)], testExecutable)
}

// testExecutable reports whether path is a Go test binary or lies under a
// test's temporary directory.
func testExecutable(path string) bool {
	if strings.HasSuffix(filepath.Base(path), ".test") {
		return true
	}
	for _, root := range []string{os.TempDir(), "/tmp"} {
		if rel, err := filepath.Rel(root, path); err == nil && strings.HasPrefix(rel, "Test") {
			return true
		}
	}
	return false
}

// ungatedMerge is a devctl merge or promotion that runs outside the gate.
type ungatedMerge struct {
	PID  int
	Line string
}

// ungatedMerges are the devctl merges and promotions of t that run outside
// the gate, by pid, each with its line (ungatedLine).
func ungatedMerges(sessions []*claude.Session, t *proc.Table) []ungatedMerge {
	owners := make(map[int]string, len(sessions))
	for _, s := range sessions {
		owners[s.PID] = s.Name
	}
	var out []ungatedMerge
	for _, pid := range slices.Sorted(maps.Keys(t.ByPID)) {
		p := t.ByPID[pid]
		if !ungatedProcess(t, p) {
			continue
		}
		out = append(out, ungatedMerge{PID: pid, Line: ungatedLine(t, p, mergeOwner(sessions, t, pid, owners))})
	}
	return out
}

// ungatedLines are the lines of ungatedMerges, for the doctor.
func ungatedLines(sessions []*claude.Session, t *proc.Table) []string {
	var out []string
	for _, m := range ungatedMerges(sessions, t) {
		out = append(out, m.Line)
	}
	return out
}

// mergeOwner is the session that runs pid: the one whose CLI is among its
// ancestors, else the one its environment names (a command whose tool shell
// exited is systemd's child), "" outside every session.
func mergeOwner(sessions []*claude.Session, t *proc.Table, pid int, owners map[int]string) string {
	if s := owner(t, pid, owners); s != "" {
		return s
	}
	if s, ok := claude.OwnerOf(sessions, pid); ok {
		return s.Name
	}
	return ""
}

// ungatedLine says a merge or promotion that runs outside the gate: its
// command, pid, session and parent before the colon (the condition's
// label), then what that means and what to do.
func ungatedLine(t *proc.Table, p *proc.Process, session string) string {
	by := noSession
	if session != "" {
		by = fmt.Sprintf("session %q", session)
	}
	parent := "no parent"
	if pp := t.ByPID[p.PPID]; pp != nil {
		parent = fmt.Sprintf("under %s (pid %d)", display(pp.Args), pp.PID)
	}
	return fmt.Sprintf("%s (pid %d, %s, %s): runs with no beekeeper gate among its ancestors, "+
		"outside the lanes, the holds and the budget floor, with devctl's own bypass; "+
		"a merge runs as its own Bash command line, which the hook gates",
		display(p.Args), p.PID, by, parent)
}

// ungated says each devctl merge or promotion that runs outside the gate,
// one UNGATED MERGE line when it is found and one ENDED line when it ends.
func (w *watcher) ungated(sessions []*claude.Session, t *proc.Table) {
	found := map[string]bool{}
	for _, m := range ungatedMerges(sessions, t) {
		key := fmt.Sprintf("%s%d", ungatedKey, m.PID)
		found[key] = true
		w.emit(key, "UNGATED MERGE %s", m.Line)
	}
	w.clearMissing(ungatedKey, found)
}
