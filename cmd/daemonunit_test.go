package cmd

import (
	"errors"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/watch"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// claudeTestBin is the claude binary the background tests name.
const claudeTestBin = "/bin/claude"

// testScoper opens a command line scopeLauncher scoped.
const testScoper = "scoper"

// scopeLauncher records the scoped command lines and the moves it is asked
// for; a move fails with err.
type scopeLauncher struct {
	platform.Launcher
	scoped    []string
	moved     [][]int
	scopes    []string
	err       error
	afterMove func([]int, string)
}

func (l *scopeLauncher) Scoped(name string, argv []string) []string {
	l.scoped = append(l.scoped, name)
	return append([]string{testScoper, name}, argv...)
}

func (l *scopeLauncher) Move(pids []int, name string) error {
	l.moved = append(l.moved, pids)
	l.scopes = append(l.scopes, name)
	err := l.err
	if err == nil && l.afterMove != nil {
		l.afterMove(pids, name)
	}
	return err
}

// Every claude --bg beekeeper runs (a start, a resume) runs in a scope of
// its own, so a daemon it starts never stays in the caller's unit; the
// claude commands that start no daemon run as they are.
func TestBackgroundCallsRunInAScopeOfTheirOwn(t *testing.T) {
	l := &scopeLauncher{}
	useLauncher(t, l)
	for _, argv := range [][]string{
		bgStartArgv(claudeTestBin, bgName, bgModel, "the brief"),
		bgResumeArgv(claudeTestBin, bgID, "your task"),
	} {
		got := bgCommand(argv, relayNow)
		if len(got) != len(argv)+2 || got[0] != testScoper || !strings.HasPrefix(got[1], daemonScope) || !slices.Equal(got[2:], argv) {
			t.Errorf("bgCommand(%q) = %q, want it in a %s* scope", argv, got, daemonScope)
		}
	}
	for _, argv := range [][]string{
		{claudeTestBin, bgStopCmd, bgID[:8]},
		{claudeTestBin, agentsName, jsonFlag},
	} {
		if got := bgCommand(argv, relayNow); !slices.Equal(got, argv) {
			t.Errorf("bgCommand(%q) = %q, want it as it is", argv, got)
		}
	}
	if len(l.scoped) != 2 {
		t.Errorf("scoped %q, want the two claude --bg calls", l.scoped)
	}
}

const (
	daemonNotifyCgroup = "/user.slice/user-1000.slice/user@1000.service/app.slice/beekeeper-notify.service"
	daemonOwnScope     = "/user.slice/user-1000.slice/user@1000.service/app.slice/claude-daemon-1.scope"
	daemonRunScope     = "/user.slice/user-1000.slice/user@1000.service/memcap.slice/memcap-1.scope"
	daemonMemLimit     = "MemoryMax 256M"
	// daemonArg and daemonRunArg are the claude daemon's subcommand and verb.
	daemonArg    = "daemon"
	daemonRunArg = "run"
)

// daemonTable is a standby watch's unit with the claude daemon a resume
// started in it, a session under the daemon, a capped test that session
// runs, a daemon client, and a second daemon in a scope of its own.
func daemonTable() (*proc.Table, map[int]string) {
	t := proc.NewTable(
		&proc.Process{PID: 20, PPID: 1, Comm: project.Name, Args: []string{project.Name, watchName, "--standby"}},
		&proc.Process{PID: 10, PPID: 1, Comm: claudeComm, Args: []string{claudeComm, daemonArg, daemonRunArg, "--origin", "transient"}},
		&proc.Process{PID: 11, PPID: 10, Comm: claudeComm, Args: []string{"claude bg-pty-host"}},
		&proc.Process{PID: 12, PPID: 11, Comm: claudeComm, Args: []string{claudeComm, "--resume", bgID}},
		&proc.Process{PID: 13, PPID: 12, Comm: "go", Args: []string{"go", "vet", "./..."}},
		&proc.Process{PID: 30, PPID: 20, Comm: claudeComm, Args: []string{claudeComm, daemonArg, "status"}},
		&proc.Process{PID: 40, PPID: 1, Comm: claudeComm, Args: []string{claudeComm, daemonArg, daemonRunArg}},
	)
	cgroups := map[int]string{20: daemonNotifyCgroup, 10: daemonNotifyCgroup, 11: daemonNotifyCgroup, 12: daemonNotifyCgroup, 13: daemonRunScope, 30: daemonNotifyCgroup, 40: daemonOwnScope}
	return t, cgroups
}

// A daemon in a beekeeper unit is found with the processes under it in that
// unit and the unit's limits; the unit's own process, a daemon client, a
// capped run of a session and a daemon in a scope of its own are not, nor is
// a daemon in a capped run's scope, which keeps its cap.
func TestDaemonsInUnits(t *testing.T) {
	table, cgroups := daemonTable()
	limits := func(cg string) []string {
		if cg == daemonNotifyCgroup {
			return []string{daemonMemLimit, "CPUQuota 50%"}
		}
		return nil
	}
	got := daemonsInUnits(table, func(pid int) string { return cgroups[pid] }, limits)
	if len(got) != 1 {
		t.Fatalf("daemons in units = %+v, want the one in beekeeper-notify.service", got)
	}
	d := got[0]
	if d.pid != 10 || d.cgroup != daemonNotifyCgroup || d.unit != "beekeeper-notify.service" || !slices.Equal(d.pids, []int{10, 11, 12}) {
		t.Errorf("daemon = %+v, want pid 10 in beekeeper-notify.service with 11 and 12", d)
	}
	if want := "the claude daemon (pid 10) and the 2 processes under it in beekeeper-notify.service (MemoryMax 256M, CPUQuota 50%)"; d.String() != want {
		t.Errorf("String = %q, want %q", d, want)
	}
	if !beekeeperUnit(daemonNotifyCgroup) || beekeeperUnit(daemonRunScope) || beekeeperUnit(daemonOwnScope) {
		t.Error("the standby unit is beekeeper's; a capped run's scope and a daemon's own scope are not")
	}
	cgroups[40] = daemonRunScope
	if got := daemonsInUnits(table, func(pid int) string { return cgroups[pid] }, limits); len(got) != 1 || got[0].pid != 10 {
		t.Errorf("daemons in units = %+v, want none in the capped run's scope", got)
	}
}

// The doctor moves a daemon it finds in a beekeeper unit into a scope of its
// own and logs it; a dry run only says it; a process that left the unit
// meanwhile stays where it is, a daemon that left it is not moved; a move
// that fails says the daemon stays, with why.
func TestDoctorMovesADaemonOutOfItsUnit(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.StateDir = dir
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: cfg, store: store, out: io.Discard, now: relayNow}
	found := []daemonUnit{{pid: 10, cgroup: daemonNotifyCgroup, unit: "beekeeper-notify.service", limits: []string{daemonMemLimit}, pids: []int{10, 11, 12}}}
	cgroups := map[int]string{10: daemonNotifyCgroup, 11: daemonNotifyCgroup, 12: daemonNotifyCgroup}
	cgroupOf := func(pid int) string { return cgroups[pid] }

	table, _ := daemonTable()
	oldMachine := plat.Machine
	t.Cleanup(func() { plat.Machine = oldMachine })
	plat.Machine = daemonSnapshotMachine{Machine: oldMachine, snapshot: func() (*proc.Table, error) { return table, nil }}
	l := &scopeLauncher{afterMove: func(pids []int, _ string) {
		for _, pid := range pids {
			cgroups[pid] = daemonOwnScope
		}
	}}
	useLauncher(t, l)
	dry := a.daemonsOutOfUnits(found, cgroupOf, true, watch.WatchParty, relayNow)
	if len(dry) != 1 || !strings.HasPrefix(dry[0], "would move the claude daemon (pid 10) and the 2 processes under it in beekeeper-notify.service (MemoryMax 256M)") || len(l.moved) != 0 {
		t.Fatalf("dry run = %q, moved %v; want a would-move line and no move", dry, l.moved)
	}

	lines := a.daemonsOutOfUnits(found, cgroupOf, false, watch.WatchParty, relayNow)
	if len(l.moved) != 1 || !slices.Equal(l.moved[0], []int{10, 11, 12}) || !strings.HasPrefix(l.scopes[0], daemonScope) || !strings.HasSuffix(l.scopes[0], "-10-beekeeper-notify.service-0") {
		t.Fatalf("moved %v into %q, want 10, 11 and 12 into a %s*-10 scope", l.moved, l.scopes, daemonScope)
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "moved the claude daemon (pid 10)") || !strings.Contains(lines[0], l.scopes[0]+".scope") {
		t.Errorf("lines = %q, want the move into %s.scope", lines, l.scopes[0])
	}
	if evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == daemonEvent }); err != nil || len(evs) != 1 {
		t.Errorf("%s events = %+v (%v), want one", daemonEvent, evs, err)
	}

	cgroups[10], cgroups[11] = daemonNotifyCgroup, daemonNotifyCgroup
	cgroups[12] = daemonOwnScope // ended, its PID reused elsewhere
	a.daemonsOutOfUnits(found, cgroupOf, false, watch.WatchParty, relayNow)
	if len(l.moved) != 2 || !slices.Equal(l.moved[1], []int{10, 11}) {
		t.Errorf("moved %v, want 10 and 11 only: 12 left the unit", l.moved)
	}
	delete(cgroups, 10) // the daemon ended
	if gone := a.daemonsOutOfUnits(found, cgroupOf, false, watch.WatchParty, relayNow); len(gone) != 0 || len(l.moved) != 2 {
		t.Errorf("a daemon that left the unit: lines %q, moved %v; want neither", gone, l.moved)
	}

	cgroups[10] = daemonNotifyCgroup
	l.err = errors.New("access denied")
	failed := a.daemonsOutOfUnits(found, cgroupOf, false, watch.WatchParty, relayNow)
	if len(failed) != 1 || !strings.Contains(failed[0], "stay there: access denied") {
		t.Errorf("failed move = %q, want the daemon staying with why", failed)
	}
}

// daemonSnapshotMachine lets a regression fork children between snapshots.
type daemonSnapshotMachine struct {
	platform.Machine
	snapshot func() (*proc.Table, error)
}

func (m daemonSnapshotMachine) Processes() (*proc.Table, error) { return m.snapshot() }

func TestDoctorRecoversChildrenBornDuringMigration(t *testing.T) {
	for _, failRecovery := range []bool{false, true} {
		t.Run(map[bool]string{false: "same tick", true: "subsequent tick"}[failRecovery], func(t *testing.T) {
			table, cgroups := daemonTable()
			cg := func(pid int) string { return cgroups[pid] }
			found := daemonsInUnits(table, cg, func(string) []string { return nil })
			// This child was absent from the doctor's initial snapshot.
			processes := slices.Collect(maps.Values(table.ByPID))
			processes = append(processes, &proc.Process{PID: 14, PPID: 12})
			table = proc.NewTable(processes...)
			cgroups[14] = daemonNotifyCgroup
			old := plat.Machine
			t.Cleanup(func() { plat.Machine = old })
			plat.Machine = daemonSnapshotMachine{Machine: old, snapshot: func() (*proc.Table, error) { return table, nil }}
			store, err := state.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			a := &app{store: store}
			l := &scopeLauncher{}
			l.afterMove = func(pids []int, _ string) {
				for _, pid := range pids {
					cgroups[pid] = daemonOwnScope
				}
				if len(l.moved) == 1 {
					// This child was forked while its parents were moving.
					processes = append(processes, &proc.Process{PID: 15, PPID: 14})
					table = proc.NewTable(processes...)
					cgroups[15] = daemonNotifyCgroup
					if failRecovery {
						l.err = errors.New("recovery refused")
					}
				}
			}
			useLauncher(t, l)
			lines := a.daemonsOutOfUnits(found, cg, false, watch.WatchParty, relayNow)
			if len(l.moved) != 2 || !slices.Equal(l.moved[0], []int{10, 11, 12, 14}) || !slices.Equal(l.moved[1], []int{15}) {
				t.Fatalf("moves = %v, want late children discovered and recovered", l.moved)
			}
			if cgroups[13] != daemonRunScope || cgroups[20] != daemonNotifyCgroup || cgroups[30] != daemonNotifyCgroup {
				t.Fatal("moved capped or unrelated processes")
			}
			if failRecovery {
				if len(lines) != 1 || !strings.Contains(lines[0], "recovery refused") {
					t.Fatalf("failure = %v", lines)
				}
				if evs, err := store.Events(0, func(e state.Event) bool { return e.Verb == daemonEvent }); err != nil || len(evs) != 0 {
					t.Fatalf("unverified success recorded: %v (%v)", evs, err)
				}
				// Next tick still finds the child although its daemon moved.
				l.err = nil
				found = daemonsInUnits(table, cg, func(string) []string { return nil })
				if len(found) != 1 || !slices.Equal(found[0].pids, []int{15}) {
					t.Fatalf("next tick = %+v", found)
				}
				lines = a.daemonsOutOfUnits(found, cg, false, watch.WatchParty, relayNow.Add(time.Second))
			}
			if len(lines) != 1 || !strings.HasPrefix(lines[0], "moved ") || len(daemonsInUnits(table, cg, func(string) []string { return nil })) != 0 {
				t.Fatalf("migration incomplete: %v, cgroups %v", lines, cgroups)
			}
		})
	}
}

// Incomplete or unreadable verification never records a successful migration.
func TestDoctorRequiresVerifiedDaemonMigration(t *testing.T) {
	for _, unreadable := range []bool{false, true} {
		t.Run(map[bool]string{false: "bounded retries", true: "unreadable snapshot"}[unreadable], func(t *testing.T) {
			table, cgroups := daemonTable()
			old := plat.Machine
			t.Cleanup(func() { plat.Machine = old })
			plat.Machine = daemonSnapshotMachine{Machine: old, snapshot: func() (*proc.Table, error) {
				if unreadable {
					return nil, errors.New("process table unreadable")
				}
				return table, nil
			}}
			l := &scopeLauncher{} // claims a move succeeded but leaves processes behind
			useLauncher(t, l)
			cg := func(pid int) string { return cgroups[pid] }
			found := daemonsInUnits(table, cg, func(string) []string { return nil })
			a := &app{} // no store: only verified successes may record an event
			lines := a.daemonsOutOfUnits(found, cg, false, watch.WatchParty, relayNow)
			want, moves := "will retry next tick", 3
			if unreadable {
				want, moves = "process table unreadable", 0
			}
			if len(lines) != 1 || !strings.Contains(lines[0], want) || len(l.moved) != moves {
				t.Fatalf("lines %v, moves %v; want %q and %d moves", lines, l.moved, want, moves)
			}
		})
	}
}

func TestDaemonMigrationIncludesForeignServicesAndScopes(t *testing.T) {
	for _, unit := range []string{"claude-bg-daemon.service", "other-agent.service", "app-desktop.scope", "session-42.scope", "claude-daemon-old.service"} {
		t.Run(unit, func(t *testing.T) {
			table, cgroups := daemonTable()
			foreign := filepath.Join(filepath.Dir(daemonNotifyCgroup), unit)
			for _, pid := range []int{10, 11, 12} {
				cgroups[pid] = foreign
			}
			cg := func(pid int) string { return cgroups[pid] }
			found := daemonsInUnits(table, cg, func(string) []string { return []string{daemonMemLimit} })
			if len(found) != 1 || found[0].unit != unit || !slices.Equal(found[0].pids, []int{10, 11, 12}) {
				t.Fatalf("foreign daemon not found: %+v", found)
			}
			old := plat.Machine
			t.Cleanup(func() { plat.Machine = old })
			plat.Machine = daemonSnapshotMachine{Machine: old, snapshot: func() (*proc.Table, error) { return table, nil }}
			store, err := state.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			l := &scopeLauncher{afterMove: func(pids []int, _ string) {
				for _, pid := range pids {
					cgroups[pid] = daemonOwnScope
				}
			}}
			useLauncher(t, l)
			a := &app{store: store}
			lines := a.daemonsOutOfUnits(found, cg, false, watch.WatchParty, relayNow)
			if len(lines) != 1 || !strings.HasPrefix(lines[0], "moved ") || len(l.moved) != 1 {
				t.Fatalf("migration: %v, moves %v", lines, l.moved)
			}
			if cgroups[13] != daemonRunScope || cgroups[20] != daemonNotifyCgroup || cgroups[30] != daemonNotifyCgroup {
				t.Fatal("moved an unrelated process or an explicitly capped build")
			}
		})
	}
}
