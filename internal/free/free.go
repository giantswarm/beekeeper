// Package free finds what the machine can give back without touching running
// work and frees it on request: dead sessions' scratch in the tmpfs /tmp,
// throwaway temp dirs and orphaned workers. What only a person decides on it
// reports with the memory the action would return: kind clusters, idle Claude
// CLIs, heavy or runaway processes and Chrome renderers. The swap reset and
// root-owned leftovers need root: beekeeper never runs as root, so they are
// printed as the commands a person runs.
package free

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// The sections --apply acts on, in the order they run and are summarized.
const (
	SessionDirs = "session-dirs"
	TmpDirs     = "tmp-dirs"
	Orphans     = "orphans"
	Swap        = "swap"
)

// Sections lists the acting sections.
var Sections = []string{SessionDirs, TmpDirs, Orphans, Swap}

// labels are the summary's section labels, shown by the front end.
var labels = map[string]string{
	SessionDirs: "dead Claude Code sessions' dirs in /tmp (tmpfs)",
	TmpDirs:     "throwaway temp dirs in /tmp",
	Orphans:     "orphaned jest / Claude workers",
	Swap:        "cold pages in swap (reset needs root)",
}

// SwapResetCmd is what a person runs to pull the swapped pages back into RAM.
const SwapResetCmd = "sudo swapoff -a && sudo swapon -a"

// ParseOnly checks a --only list against the sections.
func ParseOnly(list []string) ([]string, error) {
	for _, s := range list {
		if !slices.Contains(Sections, s) {
			return nil, fmt.Errorf("unknown section %q: want %s", s, strings.Join(Sections, ", "))
		}
	}
	return list, nil
}

// Options are the thresholds and the mode.
type Options struct {
	// Apply deletes and kills; without it free only reports.
	Apply bool
	// Summary prints the TSV candidates instead of the report (a dry run).
	Summary bool
	// Only restricts the acting sections; empty means every one.
	Only []string
	// Stale is how long a session's scratch or a temp dir must be untouched.
	Stale time.Duration
	// Orphan is how old an orphaned worker must be.
	Orphan time.Duration
	// HeavyMiB is the anonymous RSS from which a process is heavy (0: off).
	HeavyMiB int
	// RunawayCPU is the lifetime CPU share (percent) of a runaway process,
	// which has also burned at least Runaway of CPU time.
	RunawayCPU int
	Runaway    time.Duration
	// TabMiB is the anonymous RSS from which a Chrome renderer is reported
	// (0: off).
	TabMiB int
}

// Machine is what free reads of the machine, gathered by the caller.
type Machine struct {
	UID  int
	Home string
	// TmpDir holds the throwaway temp dirs; Scratch Claude Code's session
	// dirs (<TmpDir>/claude-<uid>), Projects the transcripts.
	TmpDir, Scratch, Projects string
	Table                     *proc.Table
	Sessions                  []*claude.Session
	// Titles maps CLI session ids to desktop titles, read when needed.
	Titles   func() map[string]string
	Clusters []machine.Cluster
	// ClustersErr is set when docker cannot be asked.
	ClustersErr error
	SlotDir     string
	Slots       int
	Now         time.Time
}

// Run is one invocation.
type Run struct {
	Options
	Machine
	Out io.Writer

	// Readers of the live machine, replaced in tests.
	anonKiB  func(pid int) int
	uid      func(pid int) int
	cgroup   func(pid int) string
	cwd      func(pid int) string
	mem      func() (machine.Mem, error)
	kill     func(pids []int)
	scope    func() string
	unitPIDs func(cgroup string) []int

	sumKiB   map[string]int
	sumN     map[string]int
	sudo     []string
	freedKiB int
	cwds     []string
	titles   map[string]string
}

func (r *Run) defaults() {
	if r.anonKiB == nil {
		r.anonKiB = proc.AnonKiB
	}
	if r.uid == nil {
		r.uid = proc.UID
	}
	if r.cgroup == nil {
		r.cgroup = proc.Cgroup
	}
	if r.cwd == nil {
		r.cwd = proc.Cwd
	}
	if r.mem == nil {
		r.mem = machine.ReadMem
	}
	if r.kill == nil {
		r.kill = terminate
	}
	if r.scope == nil {
		r.scope = machine.FindScope
	}
	if r.unitPIDs == nil {
		r.unitPIDs = cgroupPIDs
	}
	r.sumKiB, r.sumN = map[string]int{}, map[string]int{}
}

// Do reports, and with Apply frees, the wanted sections.
func (r *Run) Do() {
	r.defaults()
	if !r.Summary {
		r.head("before")
		r.state()
		r.slots()
	}
	r.kind()
	r.clis()
	r.heavy()
	r.tabs()
	for _, s := range Sections {
		if !r.wants(s) {
			continue
		}
		switch s {
		case SessionDirs:
			r.sessionDirs()
		case TmpDirs:
			r.tmpDirs()
		case Orphans:
			r.orphans()
		case Swap:
			r.swap()
		}
	}
	if r.Summary {
		for _, s := range Sections {
			if r.sumN[s] > 0 {
				r.emit("section", s, r.sumKiB[s]/1024, r.sumN[s], labels[s])
			}
		}
		return
	}
	r.head("after")
	r.state()
	if r.Apply {
		r.say("  freed by this run: %d MiB (tmpfs and killed workers; freed cache shows up in MemAvailable)", r.freedKiB/1024)
	} else {
		r.say("  dry run; add --apply to act")
	}
	if len(r.sudo) > 0 {
		r.head("needs root, run yourself")
		for _, c := range r.sudo {
			r.say("  %s", c)
		}
	}
}

func (r *Run) wants(section string) bool {
	return len(r.Only) == 0 || slices.Contains(r.Only, section)
}

// say prints a report line; the summary prints none.
func (r *Run) say(format string, a ...any) {
	if !r.Summary {
		_, _ = fmt.Fprintf(r.Out, format+"\n", a...)
	}
}

func (r *Run) head(title string) { r.say("\n== %s ==", title) }

// act reports an action, done or (dry run) not.
func (r *Run) act(format string, a ...any) {
	if r.Apply {
		r.say("  "+format, a...)
	} else {
		r.say("  would "+format, a...)
	}
}

// emit prints one summary row: the fields tab-separated.
func (r *Run) emit(fields ...any) {
	if !r.Summary {
		return
	}
	s := make([]string, len(fields))
	for i, f := range fields {
		s[i] = oneLine(fmt.Sprint(f))
	}
	_, _ = fmt.Fprintln(r.Out, strings.Join(s, "\t"))
}

func (r *Run) tally(section string, kib int) {
	r.sumKiB[section] += kib
	r.sumN[section]++
}

// oneLine keeps a summary field in its column.
func oneLine(s string) string {
	return strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s)
}

// uptime renders seconds as "3d 02:15" or "02:15" (hours:minutes).
func uptime(d time.Duration) string {
	s := int(d.Seconds())
	if s >= 86400 {
		return fmt.Sprintf("%dd %02d:%02d", s/86400, s%86400/3600, s%3600/60)
	}
	return fmt.Sprintf("%02d:%02d", s/3600, s%3600/60)
}

// tilde shortens a path under the home directory.
func (r *Run) tilde(p string) string {
	if r.Home != "" && (p == r.Home || strings.HasPrefix(p, r.Home+"/")) {
		return "~" + p[len(r.Home):]
	}
	return p
}

// procs returns the table's processes in pid order.
func (r *Run) procs() []*proc.Process {
	out := make([]*proc.Process, 0, len(r.Table.ByPID))
	for _, p := range r.Table.ByPID {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b *proc.Process) int { return a.PID - b.PID })
	return out
}
