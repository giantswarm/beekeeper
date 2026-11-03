//go:build unix

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// releaseWaitCmd is the command of the test's detached wait.
const releaseWaitCmd = "devctl release wait giantswarm/kagent-upstream v1.11.6"

// detachedWait leaves the files of a release wait whose merge-child runs
// (a sleep standing in for it) under dir's runs/, owned by owner, and
// returns its base and the merge-child's pid.
func detachedWait(t *testing.T, dir string, owner state.Party) (string, int) {
	t.Helper()
	c := exec.Command("sleep", "60")
	if err := c.Start(); err != nil {
		t.Skip("no sleep:", err)
	}
	t.Cleanup(func() { _ = c.Process.Kill() })
	go func() { _ = c.Wait() }() // reaped, so that its end is seen
	child := c.Process.Pid
	base := ownedBase(dir, strings.Fields(releaseWaitCmd), child)
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRunInfo(base, runInfo{Command: releaseWaitCmd, Owner: owner, Gate: 4242, Started: relayNow})
	if err := os.WriteFile(base+".pid", []byte(strconv.Itoa(child)), 0o600); err != nil {
		t.Fatal(err)
	}
	return base, child
}

// gate runs lists a detached release wait with its owner, command, start
// and merge-child; gate stop ends it within seconds, logs who stopped it
// and leaves the stop for the merge-child's outcome.
func TestGateStopEndsADetachedReleaseWait(t *testing.T) {
	w, _, _ := notifyingWatch(t, t.TempDir(), false)
	a := w.app
	base, child := detachedWait(t, a.store.Dir(), four)
	finished, _ := detachedWait(t, a.store.Dir(), four)
	if err := os.WriteFile(finished+".rc", []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	runs := detachedRuns(a.store.Dir(), st, nil)
	if len(runs) != 1 {
		t.Fatalf("runs: %+v, want the running wait alone", runs)
	}
	r := runs[0]
	if r.ID != filepath.Base(base) || r.Kind != runWait || r.Phase != state.Running || r.Command != releaseWaitCmd ||
		r.Owner != agentFour || r.Session != four.Session || r.PID != child || !r.Since.Equal(relayNow) {
		t.Errorf("the wait's row: %+v", r)
	}
	stopper := state.Party{Session: "s9", Name: supervisorName}
	said, err := a.gateStop(stopper, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := "stopped " + r.ID + ": " + releaseWaitCmd + " (owner " + agentFour + "), its merge-child (pid " + strconv.Itoa(child) + ") is ended"; !strings.HasPrefix(said, want) {
		t.Errorf("said %q, want %q", said, want)
	}
	if proc.Alive(child) {
		t.Error("the stopped wait's merge-child runs on")
	}
	if by := readStopped(base); !by.Is(stopper) {
		t.Errorf("the stop is marked for %+v, want the stopper", by)
	}
	if d := lastEventOf(t, a, "devctl.stopped"); !strings.Contains(d, r.ID+": "+releaseWaitCmd+" (owner "+agentFour+") stopped (gate stop)") {
		t.Errorf("devctl.stopped event: %q", d)
	}
	if _, err := a.gateStop(stopper, r.ID); Code(err) != ExitRefused {
		t.Errorf("a second stop: exit %d (%v), want refused", Code(err), err)
	}
}

// gate stop ends a merge as a TaskStop of its task does: a running merge's
// devctl through its merge-child, the merge left to its gate to record; a
// queued merge's own run waiting for its turn leaves its lane, a seeded
// place too. The queued run's files under runs/ are the merge's, not a
// wait of their own.
func TestGateStopEndsARunningAndAQueuedMerge(t *testing.T) {
	w, _, _ := notifyingWatch(t, t.TempDir(), false)
	a := w.app
	dir := a.store.Dir()
	stubPulls(t, map[int]github.Pull{1: {State: github.Open}})
	child := hungChild(t, dir, hungRepo, 1)
	run, _ := detachedWait(t, dir, four)
	queued := sleeper(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = []state.Merge{
			{Repo: hungRepo, PR: 1, Lane: hungLane, By: four, PID: 1, Child: child, Phase: state.Running, Started: relayNow},
			{Repo: nextRepo, PR: 4, Lane: hungLane, By: four, PID: queued, Run: run, Phase: state.Waiting, Seeded: true, Joined: relayNow.Add(time.Minute)},
			{Repo: nextRepo, PR: 5, Lane: hungLane, By: four, PID: sleeper(t), Phase: state.Waiting, Joined: relayNow},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	runs := detachedRuns(dir, st, nil)
	if len(runs) != 2 || runs[0].PR != 1 || runs[0].PID != child || runs[0].Command != "devctl pr merge o/hung 1" ||
		runs[1].PR != 4 || runs[1].PID != queued || runs[1].Phase != state.Waiting || runs[1].Command != releaseWaitCmd {
		t.Fatalf("runs: %+v, want the running merge and the queued run, not its files as a wait nor a merge waiting in its caller", runs)
	}
	stopper := state.Party{Session: "s9", Name: supervisorName}
	said, err := a.gateStop(stopper, runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := "o/hung#1 cancelled, GitHub reports it open: its devctl (merge-child pid " + strconv.Itoa(child) + ") is ended, nothing merged"; !strings.Contains(said, want) {
		t.Errorf("said %q, want %q", said, want)
	}
	if !eventually(5*time.Second, func() bool { return !proc.Alive(child) }) {
		t.Error("the stopped merge's merge-child runs on")
	}
	said, err = a.gateStop(stopper, runs[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := "o/next#4 cancelled before its turn, nothing merged; it leaves lane " + hungLane; !strings.Contains(said, want) {
		t.Errorf("said %q, want %q", said, want)
	}
	if st, err = a.store.Read(); err != nil {
		t.Fatal(err)
	}
	if len(st.Merges) != 2 || st.Merges[0].Phase != state.Running || st.Merges[1].PR != 5 {
		t.Errorf("want the running merge, for its gate to record, and the merge waiting in its caller: %+v", st.Merges)
	}
	if d := lastEventOf(t, a, "merge.stopped"); !strings.Contains(d, `o/next#4: stopped by "`+supervisorName+`" (gate stop), it leaves lane `+hungLane) {
		t.Errorf("merge.stopped event: %q", d)
	}
}

// A wait gate stop ended tells its owner who stopped it, and nothing is
// owed to an owner who stopped it.
func TestAStoppedWaitsOutcomeNamesTheStopper(t *testing.T) {
	r := childResult{spec: childSpec{Argv: strings.Fields(releaseWaitCmd), Owner: four}, rc: 128 + int(syscall.SIGTERM),
		stop: syscall.SIGTERM, stoppedBy: state.Party{Session: "s9", Name: supervisorName}}
	if got := r.outcome(); !strings.HasPrefix(got, releaseWaitCmd+` did not settle: "`+supervisorName+`" stopped it (beekeeper gate stop, exit 143)`) {
		t.Errorf("outcome %q", got)
	}
	if done := r.done().result(); !done.stoppedBy.Is(r.stoppedBy) {
		t.Errorf("the stopper does not survive a re-exec: %+v", done.stoppedBy)
	}
	w, _, _ := notifyingWatch(t, t.TempDir(), false)
	r.stoppedBy = four
	w.tellOwner(t.Context(), r)
	if st, err := w.store.Read(); err != nil || len(st.Owed) != 0 {
		t.Errorf("owed to the owner who stopped it: %+v (%v)", st.Owed, err)
	}
}

// gate runs and gate stop are the gate's subcommands; a gated devctl
// command still reaches the gate itself.
func TestGateRoutesItsSubcommands(t *testing.T) {
	a := &app{}
	c := a.gateCmd()
	// A subcommand's name is the first word; anything else reaches the gate.
	for args, sub := range map[string]bool{
		"runs":                               true,
		"stop x":                             true,
		"--wait 1m -- devctl pr merge o/r 7": false,
		"-- devctl release wait o/r v1.0.0":  false,
		"devctl pr wait o/r 7":               false,
	} {
		want := gateCmdName
		if sub {
			want = strings.Fields(args)[0]
		}
		found, _, err := c.Find(strings.Fields(args))
		if err != nil || found.Name() != want {
			t.Errorf("%q reaches %v (%v), want %s", args, found.Name(), err, want)
		}
	}
}
