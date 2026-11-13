package cmd

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// writeRunInfo leaves i for gate runs in base.run, written whole.
func writeRunInfo(base string, i runInfo) {
	raw, _ := json.Marshal(i)
	if os.WriteFile(base+runInfoExt+".tmp", raw, 0o600) == nil { //nolint:gosec // the gate's own file
		_ = os.Rename(base+runInfoExt+".tmp", base+runInfoExt) //nolint:gosec // as above
	}
}

// readStopped is who stopped the run at base with gate stop, zero for
// nobody.
func readStopped(base string) state.Party {
	var p state.Party
	if raw, err := os.ReadFile(base + stoppedExt); err == nil { //nolint:gosec // the gate's own file
		_ = json.Unmarshal(raw, &p)
	}
	return p
}

// Kinds of a detached run.
const (
	runMerge = "merge"
	runWait  = "wait"
)

// detachedRun is one run the gate runs outside its caller: a merge or
// promotion running or waiting in a queued run of its own, or a devctl
// wait. ID is the base name of its files, which gate stop takes; PID the
// merge-child that runs devctl, for a queued merge still waiting its gate
// under --queued; Devctl the devctl under it, 0 before it started.
type detachedRun struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Phase   string    `json:"phase"`
	Command string    `json:"command"`
	Repo    string    `json:"repo,omitempty"`
	PR      int       `json:"pr,omitempty"`
	Owner   string    `json:"owner,omitempty"`
	Session string    `json:"session,omitempty"`
	Since   time.Time `json:"since,omitzero"`
	PID     int       `json:"pid"`
	Devctl  int       `json:"devctl,omitempty"`
	base    string
}

// Phases of a detached run besides the merge phases.
const (
	// runLeft is a wait its call left to run on outside it (leftExt).
	runLeft = "left"
	runDead = "dead"
)

// detachedRuns lists the detached runs of the state directory dir: the
// running merges whose merge-child runs and the waiting merges with a
// queued run of their own whose gate runs, from st, then the waits under
// runs/ without an exit code, including dead children, the oldest first. t
// finds each run's devctl; nil leaves it out.
func detachedRuns(dir string, st *state.State, t *proc.Table) []detachedRun {
	var runs []detachedRun
	covered := map[string]bool{}
	for _, m := range st.Merges {
		if m.Run != "" {
			covered[m.Run] = true
		}
		base := mergeBase(dir, m.Repo, m.PR)
		r := detachedRun{ID: filepath.Base(base), Kind: runMerge, Phase: m.Phase, Repo: m.Repo, PR: m.PR,
			Owner: m.By.Name, Session: m.By.Session, base: base}
		switch {
		case m.Phase == state.Running && m.Child != 0 && proc.Alive(m.Child):
			r.Since, r.PID = m.Started, m.Child
		case m.Phase == state.Waiting && m.Run != "" && m.PID != 0 && proc.Alive(m.PID):
			r.Since, r.PID = m.Joined, m.PID
		default:
			continue
		}
		r.Command = mergeCommand(m)
		info := base // a waiting merge's queued run, its gate under --queued
		if m.Phase == state.Waiting {
			info = m.Run
		}
		if i, ok := readRunInfo(info); ok {
			r.Command = i.Command
		}
		r.Devctl = devctlUnder(t, r.PID)
		runs = append(runs, r)
	}
	pids, _ := filepath.Glob(filepath.Join(dir, ownedRuns, "*.pid"))
	for _, f := range pids {
		base := strings.TrimSuffix(f, ".pid")
		pid := readPID(base)
		if covered[base] || pid == 0 {
			continue
		}
		if _, err := os.Stat(base + ".rc"); err == nil {
			continue
		}
		r := detachedRun{ID: filepath.Base(base), Kind: runWait, Phase: state.Running, PID: pid, Devctl: devctlUnder(t, pid), base: base}
		if leftToRunOn(base) {
			r.Phase = runLeft
		}
		if i, ok := readRunInfo(base); ok {
			r.Command, r.Owner, r.Session, r.Since = i.Command, i.Owner.Name, i.Owner.Session, i.Started
		} else if info, err := os.Stat(f); err == nil {
			r.Since = info.ModTime().UTC()
		}
		if !runProcessAlive(base, pid, t) {
			r.Phase, r.Devctl = runDead, 0
		}
		if r.Command == "" && t != nil && t.ByPID[r.Devctl] != nil {
			r.Command, _ = masked(t.ByPID[r.Devctl].Args)
		}
		runs = append(runs, r)
	}
	slices.SortStableFunc(runs, func(a, b detachedRun) int { return a.Since.Compare(b.Since) })
	return runs
}

// mergeCommand is the devctl command of merge m, as the gate runs it.
func mergeCommand(m state.Merge) string {
	if m.PR == 0 {
		return strings.Join([]string{merge.Tool, "release", "promote", m.Repo}, " ")
	}
	return fmt.Sprintf("%s pr merge %s %d", merge.Tool, m.Repo, m.PR)
}

// devctlUnder is the devctl process under pid, 0 for none or without t.
func devctlUnder(t *proc.Table, pid int) int {
	if t == nil {
		return 0
	}
	for _, p := range t.Descendants(pid) {
		if p.Comm == merge.Tool {
			return p.PID
		}
	}
	return 0
}

func (a *app) gateRunsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "runs",
		Short: "The runs the gate runs outside their callers: merges, promotions and devctl waits",
		Long: `runs lists what the gate runs outside its callers, which a TaskStop or a
killed shell does not end: a merge or promotion whose devctl runs (its
merge-child) or that waits for its turn in a queued run of its own (exit
76), and a devctl pr wait, release wait or rollout wait, also one its call
left to run on (phase left). A wait whose merge-child is gone or whose pid
was reused has phase dead; the watch moves its files aside after a day. Each row is its id, which gate stop takes, the
kind and phase, the owning session, the command, since when it runs or
waits, its pid (the merge-child, or a queued merge's waiting gate) and its
devctl's. End one with gate stop, never a kill by pid.`,
		Args: cobra.NoArgs,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return a.load()
		},
		RunE: func(*cobra.Command, []string) error {
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			t, _ := plat.Machine.Processes()
			runs := detachedRuns(a.store.Dir(), st, t)
			if a.json {
				return a.printJSON(runs)
			}
			if len(runs) == 0 {
				_, err := fmt.Fprintln(a.out, "no detached runs")
				return err
			}
			now := clk.Now()
			w := a.table()
			_, _ = fmt.Fprintln(w, "ID\tKIND\tPHASE\tOWNER\tSINCE\tPID\tDEVCTL\tCOMMAND")
			for _, r := range runs {
				since := "-"
				if !r.Since.IsZero() {
					since = r.Since.Local().Format("15:04") + " (" + dur(now.Sub(r.Since)) + ")"
				}
				devctl := "-"
				if r.Devctl != 0 {
					devctl = fmt.Sprint(r.Devctl)
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", r.ID, r.Kind, r.Phase, cmp.Or(r.Owner, "-"), since, r.PID, devctl, r.Command)
			}
			return w.Flush()
		},
	}
}

func (a *app) gateStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <id>",
		Short: "End a run the gate runs outside its caller, and its devctl",
		Long: `stop ends one run gate runs lists, by its id, and logs who stopped it. A
merge waiting in a queued run of its own leaves its lane (a seeded place
too): its gate refuses (exit 77) and wakes its owner, nothing merged. A
running merge's devctl is ended through its merge-child, as a TaskStop of
its task ends it, and the stop says whether the pull request merged before
it, as GitHub reports it; its gate, or the watch once the gate is gone,
records the run (merge.stopped). A wait's merge-child is sent SIGTERM, which
it passes on to devctl, and devctl is killed when it has not ended
stopWait (5s) later; the wait's caller, if it still follows, reads exit 143,
its owner is woken with the stop unless the owner stopped it, and every run
that followed it ends with it (devctl.stopped). Only a pid the run recorded
itself is signalled. Its devctl gone, the run holds no lane place and no
devctl slot any more.`,
		Args: cobra.ExactArgs(1),
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return a.load()
		},
		RunE: func(_ *cobra.Command, args []string) error {
			me := a.gateCaller("", 0, nil, false)
			said, err := a.gateStop(me, args[0])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, said)
			return err
		},
	}
}

// gateStop stops the detached run id (gate stop) as by and says how it
// ended.
func (a *app) gateStop(by state.Party, id string) (string, error) {
	st, err := a.store.Read()
	if err != nil {
		return "", err
	}
	t, _ := plat.Machine.Processes()
	runs := detachedRuns(a.store.Dir(), st, t)
	i := slices.IndexFunc(runs, func(r detachedRun) bool { return r.ID == id })
	if i < 0 {
		return "", refused("no detached run %s: beekeeper gate runs lists them", id)
	}
	r := runs[i]
	if r.Kind == runWait {
		return a.stopWait(by, r)
	}
	said, err := stopMerges(a.store, by, func(m state.Merge) bool {
		return m.Repo == r.Repo && m.PR == r.PR && m.Phase == r.Phase
	}, false, func(state.Merge) string {
		return fmt.Sprintf("stopped by %q (gate stop)", by.Name)
	})
	if err != nil {
		return "", err
	}
	if len(said) == 0 {
		return "", refused("%s ended before the stop: beekeeper gate runs lists what runs", id)
	}
	return "stopped " + id + ": " + strings.Join(said, "; "), nil
}

// stopWait ends the wait r as by: its merge-child, which passes the SIGTERM
// on to devctl, then devctl by SIGKILL when the merge-child still runs
// stopWait later. It signals only the pid the run recorded, and the devctl
// processes under it.
func (a *app) stopWait(by state.Party, r detachedRun) (string, error) {
	t, err := plat.Machine.Processes()
	if err != nil {
		return "", err
	}
	if readPID(r.base) != r.PID || !runProcessAlive(r.base, r.PID, t) {
		return "", refused("%s ended before the stop: beekeeper gate runs lists what runs", r.ID)
	}
	raw, _ := json.Marshal(by)
	if err := os.WriteFile(r.base+stoppedExt, raw, 0o600); err != nil { //nolint:gosec // the gate's own file
		return "", err
	}
	if err := endChild(r.PID); err != nil {
		return "", err
	}
	how := "is ended"
	if !waitGone(r.PID, stopWait) {
		killed := 0
		if t, err := plat.Machine.Processes(); err == nil {
			for _, p := range t.Descendants(r.PID) {
				if p.Comm == merge.Tool {
					signalPID(p.PID, syscall.SIGKILL)
					killed++
				}
			}
		}
		how = fmt.Sprintf("did not end within %s, its devctl was killed", stopWait)
		if killed == 0 || !waitGone(r.PID, stopWait) {
			how = fmt.Sprintf("still runs %s after SIGTERM", 2*stopWait)
		}
	}
	owner := cmp.Or(r.Owner, "nobody")
	_ = a.store.Record(event(by, "devctl.stopped", "%s: %s (owner %s) stopped (gate stop), its merge-child (pid %d) %s", r.ID, r.Command, owner, r.PID, how))
	return fmt.Sprintf("stopped %s: %s (owner %s), its merge-child (pid %d) %s; nothing waits for it any more", r.ID, r.Command, owner, r.PID, how), nil
}

// runProcessAlive checks identity as well as liveness. Old records predate
// processStarted: require the merge-child command for this exact base and
// reject processes started after the record was written.
func runProcessAlive(base string, pid int, t *proc.Table) bool {
	if t == nil {
		var err error
		t, err = plat.Machine.Processes()
		if err != nil {
			return proc.Alive(pid)
		} // do not retire work on a failed scan
	}
	p := t.Process(pid)
	if p == nil {
		return false
	}
	i, _ := readRunInfo(base)
	if !i.ProcessStarted.IsZero() {
		return p.Start.Equal(i.ProcessStarted)
	}
	since := i.Started
	if since.IsZero() {
		if f, err := os.Stat(base + ".pid"); err == nil {
			since = f.ModTime()
		}
	}
	if !since.IsZero() && p.Start.After(since) {
		return false
	}
	for n, arg := range p.Args {
		if arg == mergeChildCmd && n+1 < len(p.Args) && p.Args[n+1] == base {
			return true
		}
	}
	return false
}

// cleanupDeadRuns moves old dead waits aside, preserving their files for
// inspection. A failed process scan leaves everything in place.
func cleanupDeadRuns(dir string, st *state.State, now time.Time) {
	t, err := plat.Machine.Processes()
	if err != nil {
		return
	}
	for _, r := range detachedRuns(dir, st, t) {
		if r.Phase != runDead || r.Since.IsZero() || now.Sub(r.Since) <= 24*time.Hour {
			continue
		}
		if runProcessAlive(r.base, r.PID, nil) {
			continue
		}
		target := filepath.Join(dir, keptOutputs, r.ID+"-dead-"+now.UTC().Format("20060102T150405Z"))
		if os.MkdirAll(target, 0o700) != nil {
			continue
		}
		for _, ext := range mergeExts {
			_ = os.Rename(r.base+ext, filepath.Join(target, r.ID+ext))
		}
	}
}
