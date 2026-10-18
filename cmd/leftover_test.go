package cmd

import (
	"slices"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	leftoverCLI     = 100
	leftoverWait    = 200
	leftoverSleep   = 201
	leftoverOwn     = 300
	leftoverSelf    = 301
	leftoverForward = 400
	leftoverKubectl = 401

	sleepComm = "sleep"
)

// leftoverTable is a worker's tool shells: a background wait loop asleep,
// with own the shell of the calling beekeeper command, with forward a
// background port-forward; with cli their CLI.
func leftoverTable(cli, own, forward bool) *proc.Table {
	shell := func(pid int) *proc.Process {
		return &proc.Process{PID: pid, PPID: leftoverCLI, Comm: testShell, Args: []string{"/bin/" + testShell, "-c", "source /home/u/.claude/shell-snapshots/snapshot-zsh-1.sh && eval '...'"}}
	}
	ps := []*proc.Process{
		shell(leftoverWait),
		{PID: leftoverSleep, PPID: leftoverWait, Comm: sleepComm, Args: []string{sleepComm, "30"}},
	}
	if own {
		ps = append(ps, shell(leftoverOwn),
			&proc.Process{PID: leftoverSelf, PPID: leftoverOwn, Comm: pluginName, Args: []string{pluginName, agentsName}})
	}
	if cli {
		ps = append(ps, &proc.Process{PID: leftoverCLI, PPID: 1, Comm: claudeComm, Args: []string{claudeComm, sessionIDFlag, "w1"}})
	}
	if forward {
		ps = append(ps, shell(leftoverForward),
			&proc.Process{PID: leftoverKubectl, PPID: leftoverForward, Comm: "kubectl", Args: []string{"kubectl", "port-forward", "svc/x", "8080"}})
	}
	return proc.NewTable(ps...)
}

func captureStops(t *testing.T) *[]int {
	t.Helper()
	var stopped []int
	orig := stopProcesses
	stopProcesses = func(pids []int) { stopped = append(stopped, pids...) }
	t.Cleanup(func() { stopProcesses = orig })
	return &stopped
}

// agents idle --done stops every tool shell its session left running, the
// processes below them included, and never the shell it runs under.
func TestStopBackgroundStopsAllButTheCallersShell(t *testing.T) {
	stopped := captureStops(t)
	cmds := stopBackground(leftoverTable(true, true, true), leftoverSelf, time.Now())
	slices.Sort(*stopped)
	if want := []int{leftoverWait, leftoverSleep, leftoverForward, leftoverKubectl}; !slices.Equal(*stopped, want) {
		t.Errorf("stopped %v, want %v", *stopped, want)
	}
	if len(cmds) != 2 {
		t.Errorf("reported %v, want the sleep and the port-forward", cmds)
	}
}

// A caller whose CLI the table does not show (a sandbox's own process
// namespace) stops nothing.
func TestStopBackgroundWithoutItsCLI(t *testing.T) {
	stopped := captureStops(t)
	if cmds := stopBackground(leftoverTable(false, true, true), leftoverSelf, time.Now()); len(cmds) > 0 || len(*stopped) > 0 {
		t.Errorf("stopped %v (%v) with no CLI in the table", *stopped, cmds)
	}
}

// The doctor finds a finished agent whose CLI only sleeps in a wait it left
// behind, and no agent whose CLI does anything else.
func TestSleepingWaits(t *testing.T) {
	now := time.Now()
	worker := state.Party{Name: "w1", Session: "w1"}
	none := func(state.Party) bool { return false }
	for _, c := range []struct {
		desc     string
		done     bool
		forward  bool
		active   time.Duration
		inFlight bool
		want     bool
	}{
		{desc: "a done agent asleep in a wait", done: true, active: time.Hour, want: true},
		{desc: "an agent not done", active: time.Hour},
		{desc: "a done agent with a port-forward too", done: true, forward: true, active: time.Hour},
		{desc: "a done agent writing its transcript", done: true, active: time.Second},
		{desc: "a done agent with a gated merge in flight", done: true, active: time.Hour, inFlight: true},
	} {
		t.Run(c.desc, func(t *testing.T) {
			tab := leftoverTable(true, false, c.forward)
			st := &state.State{Agents: []state.Agent{{Party: worker, Done: c.done}}}
			sessions := []*claude.Session{{PID: leftoverCLI, ID: "w1", Name: "w1", LastActive: now.Add(-c.active)}}
			inFlight := none
			if c.inFlight {
				inFlight = func(state.Party) bool { return true }
			}
			waits := sleepingWaits(st, sessions, tab, inFlight, now)
			if got := len(waits) == 1; got != c.want {
				t.Fatalf("waits %v, want one: %v", waits, c.want)
			}
			if c.want && !slices.Equal(waits[0].pids(), []int{leftoverWait, leftoverSleep}) {
				t.Errorf("pids %v", waits[0].pids())
			}
		})
	}
}
