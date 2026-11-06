package cmd

import (
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// daemonUnit is a claude daemon that runs in the cgroup of a beekeeper unit:
// a claude --bg that unit ran started it there. Every background session
// then shares that unit's limits, and a stop or restart of the unit ends
// them all.
type daemonUnit struct {
	pid int
	// cgroup is the unit's cgroup path, unit its last element; limits are
	// its limits ("MemoryMax 256M").
	cgroup string
	unit   string
	limits []string
	// pids are the daemon and the processes under it in the unit's cgroup,
	// the daemon first.
	pids []int
}

// String names the daemon, what runs under it and the unit with its limits.
func (d daemonUnit) String() string {
	unit := d.unit
	if len(d.limits) > 0 {
		unit += " (" + strings.Join(d.limits, ", ") + ")"
	}
	return fmt.Sprintf("the claude daemon (pid %d) and the %d processes under it in %s", d.pid, len(d.pids)-1, unit)
}

// risk is what the unit means to the daemon's background sessions while it
// runs there, gone says it no longer does.
func (d daemonUnit) risk(gone bool) string {
	switch {
	case gone && len(d.limits) > 0:
		return "its background sessions no longer share that unit's limits or end with its stop"
	case gone:
		return "its background sessions no longer end with that unit's stop"
	case len(d.limits) > 0:
		return "its background sessions share that unit's limits, and a stop or restart of the unit ends them all"
	}
	return "a stop or restart of the unit ends all its background sessions"
}

// daemonsInUnits are the claude daemons of t that run in a beekeeper unit's
// cgroup; cgroupOf reads a process's cgroup path, limitsOf a cgroup's
// limits.
func daemonsInUnits(t *proc.Table, cgroupOf func(pid int) string, limitsOf func(cgroup string) []string) []daemonUnit {
	var out []daemonUnit
	for _, pid := range slices.Sorted(maps.Keys(t.ByPID)) {
		if !claude.IsDaemon(t.ByPID[pid]) {
			continue
		}
		cg := cgroupOf(pid)
		if !beekeeperUnit(cg) {
			continue
		}
		d := daemonUnit{pid: pid, cgroup: cg, unit: path.Base(cg), limits: limitsOf(cg), pids: []int{pid}}
		for _, p := range t.Descendants(pid) {
			if cgroupOf(p.PID) == cg {
				d.pids = append(d.pids, p.PID)
			}
		}
		out = append(out, d)
	}
	return out
}

// beekeeperUnit reports whether the cgroup path cg is a unit beekeeper
// starts or installs (ownUnits). A capped run's scope is none: what runs
// there keeps its cap.
func beekeeperUnit(cg string) bool {
	ok, _ := path.Match(ownUnits, path.Base(cg))
	return ok
}

// liveDaemonsInUnits reads daemonsInUnits from the machine's cgroups; tests
// replace it.
var liveDaemonsInUnits = func(t *proc.Table) []daemonUnit {
	return daemonsInUnits(t, proc.Cgroup, func(cg string) []string {
		return machine.Limits(filepath.Join(machine.CgroupRoot, cg))
	})
}

// daemonEvent is the event of a claude daemon the doctor moved.
const daemonEvent = "daemon.move"

// daemonsOutOfUnits moves each claude daemon found in a beekeeper unit, with
// the processes under it there, into a transient scope of its own under
// app.slice, where no unit's limits or stop reach its background sessions,
// and says each in one line; with dryRun it only says it. Moving a process
// to another cgroup leaves it running: no session is stopped. Right before
// the move cgroupOf reads each process's cgroup again: one that left the
// unit's (an ended process, its PID reused elsewhere) stays where it is, and
// a daemon no longer there is not moved.
func (a *app) daemonsOutOfUnits(found []daemonUnit, cgroupOf func(pid int) string, dryRun bool, by state.Party, now time.Time) []string {
	var out []string
	for _, d := range found {
		if dryRun {
			out = append(out, fmt.Sprintf("would move %s into a scope of its own: %s", d, d.risk(false)))
			continue
		}
		d.pids = slices.DeleteFunc(slices.Clone(d.pids), func(pid int) bool { return cgroupOf(pid) != d.cgroup })
		if len(d.pids) == 0 || d.pids[0] != d.pid {
			continue // the daemon ended or left the unit meanwhile
		}
		scope := daemonScopeName(now) + "-" + strconv.Itoa(d.pid)
		if err := plat.Launcher.Move(d.pids, scope); err != nil {
			out = append(out, fmt.Sprintf("%s stay there: %v; %s", d, err, d.risk(false)))
			continue
		}
		line := fmt.Sprintf("moved %s into %s.scope: %s", d, scope, d.risk(true))
		_ = a.store.Record(event(by, daemonEvent, "%s", line))
		out = append(out, line)
	}
	return out
}
