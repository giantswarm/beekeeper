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

// daemonUnit is a claude daemon with processes still in an unsafe unit.
// The daemon may already have moved while descendants stayed behind.
type daemonUnit struct {
	pid int
	// cgroup is the unit's cgroup path, unit its last element; limits are
	// its limits ("MemoryMax 256M").
	cgroup string
	unit   string
	limits []string
	// pids are processes under the daemon in this unit, including the
	// daemon first when it still runs there.
	pids []int
}

// String names the daemon, what runs under it and the unit with its limits.
func (d daemonUnit) String() string {
	unit := d.unit
	if len(d.limits) > 0 {
		unit += " (" + strings.Join(d.limits, ", ") + ")"
	}
	children := len(d.pids)
	if slices.Contains(d.pids, d.pid) {
		children--
	}
	return fmt.Sprintf("the claude daemon (pid %d) and the %d processes under it in %s", d.pid, children, unit)
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

// daemonsInUnits finds daemons and their descendants left in services or foreign scopes,
// units, even when a prior migration already moved the daemon itself.
func daemonsInUnits(t *proc.Table, cgroupOf func(pid int) string, limitsOf func(cgroup string) []string) []daemonUnit {
	var out []daemonUnit
	for _, pid := range slices.Sorted(maps.Keys(t.ByPID)) {
		if !claude.IsDaemon(t.ByPID[pid]) {
			continue
		}
		cg := cgroupOf(pid)
		if !unsafeDaemonUnit(cg) && !ownDaemonScope(cg) {
			continue // separately capped daemons keep their cap
		}
		groups := map[string][]int{}
		if unsafeDaemonUnit(cg) {
			groups[cg] = []int{pid}
		}
		for _, p := range t.Descendants(pid) {
			if childCG := cgroupOf(p.PID); unsafeDaemonUnit(childCG) {
				groups[childCG] = append(groups[childCG], p.PID)
			}
		}
		for _, group := range slices.Sorted(maps.Keys(groups)) {
			out = append(out, daemonUnit{pid: pid, cgroup: group, unit: path.Base(group), limits: limitsOf(group), pids: groups[group]})
		}
	}
	return out
}

func ownDaemonScope(cg string) bool {
	unit := path.Base(cg)
	return strings.HasPrefix(unit, daemonScope) && strings.HasSuffix(unit, ".scope")
}

// Services have a main PID whose clean exit can kill an upgrading daemon.
// Foreign scopes end with their app or session. Explicit build caps stay put.
func unsafeDaemonUnit(cg string) bool {
	unit := path.Base(cg)
	if ownDaemonScope(cg) || strings.HasPrefix(unit, "memcap-") && strings.HasSuffix(unit, ".scope") {
		return false
	}
	return strings.HasSuffix(unit, ".service") || strings.HasSuffix(unit, ".scope")
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

// daemonsOutOfUnits rescans before every move and verifies with a fresh
// process table afterwards. Children forked while parents move are recovered
// in another scope. A bounded retry leaves a warning; discovery on the next
// doctor tick also finds stranded children whose daemon already left the unit.
func (a *app) daemonsOutOfUnits(found []daemonUnit, cgroupOf func(pid int) string, dryRun bool, by state.Party, now time.Time) []string {
	var out []string
	for _, d := range found {
		if dryRun {
			out = append(out, fmt.Sprintf("would move %s into a scope of its own: %s", d, d.risk(false)))
			continue
		}
		if line := a.moveDaemonUnit(d, cgroupOf, by, now); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// remainingDaemonUnit deliberately discovers descendants even after the root
// daemon has moved. Reading only the old PID list can never find new children.
func remainingDaemonUnit(d daemonUnit, cgroupOf func(int) string) ([]int, error) {
	table, err := plat.Machine.Processes()
	if err != nil {
		return nil, err
	}
	for _, current := range daemonsInUnits(table, cgroupOf, func(string) []string { return nil }) {
		if current.pid == d.pid && current.cgroup == d.cgroup {
			return current.pids, nil
		}
	}
	return nil, nil
}

func (a *app) moveDaemonUnit(d daemonUnit, cgroupOf func(int) string, by state.Party, now time.Time) string {
	var scopes []string
	for attempt := range 4 {
		pids, err := remainingDaemonUnit(d, cgroupOf)
		if err != nil {
			return fmt.Sprintf("cannot verify %s: %v; %s", d, err, d.risk(false))
		}
		// Recheck immediately before the move: ended processes, reused PIDs
		// elsewhere and separately capped runs stay where they are.
		pids = slices.DeleteFunc(pids, func(pid int) bool { return cgroupOf(pid) != d.cgroup })
		if len(pids) == 0 {
			if len(scopes) == 0 {
				return ""
			}
			line := fmt.Sprintf("moved %s into %s: %s", d, strings.Join(scopes, ", "), d.risk(true))
			_ = a.store.Record(event(by, daemonEvent, "%s", line))
			return line
		}
		if attempt == 3 {
			return fmt.Sprintf("%s still has processes %v in its unit after migration; will retry next tick; %s", d, pids, d.risk(false))
		}
		scope := daemonScopeName(now) + "-" + strconv.Itoa(d.pid) + "-" + d.unit + "-" + strconv.Itoa(attempt)
		if err := plat.Launcher.Move(pids, scope); err != nil {
			return fmt.Sprintf("%s stay there: %v; %s", d, err, d.risk(false))
		}
		scopes = append(scopes, scope+".scope")
	}
	return ""
}
