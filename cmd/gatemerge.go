package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// Seams for the tests.
var (
	toolRelease = func(ctx context.Context, sha string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return github.CommitRelease(ctx, github.RunGH, merge.ToolRepo, sha)
	}
	// mergeAuthority verifies that the pull request head being merged has an
	// approved desk review or an explicit exact-head supervisor go.
	mergeAuthority = gateMergeHeadAuthority
	updateRebase   = func(ctx context.Context, repo string, n int) error {
		return github.UpdateByRebase(ctx, github.RunGH, repo, n)
	}
	updateBranch = func(ctx context.Context, repo string, n int) error {
		return github.UpdateBranch(ctx, github.RunGH, repo, n)
	}
	devctlUpdate = toolUpdate
	// toolCandidate is the newest release candidate of the merge tool
	// published since a time with this platform's binary uploaded, "" for
	// none.
	toolCandidate = func(ctx context.Context, since time.Time) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return github.PublishedCandidate(ctx, github.RunGH, merge.ToolRepo, merge.Tool+"-"+runtime.GOOS+"-"+runtime.GOARCH, since)
	}
	userSystemd   = plat.Launcher.Available
	runningBinary = func() Binary { return liveBinary{platform.RunningBinary()} }
	gateOutput    = taskOutput
)

// taskOutput is the file of the Claude Code background task this process
// runs in, which a TaskStop names: its own stdout when that is a regular
// file, else that of its nearest ancestor below the Claude Code CLI writing
// to <tasks>/<task id>.output, the task's shell when the gate's output is
// piped on (`| tail`). "" for none, or where the platform cannot say.
func taskOutput() string {
	if p := stdoutOf(os.Getpid()); p != "" {
		return p
	}
	t, err := plat.Machine.Processes()
	if err != nil {
		return ""
	}
	return ancestorOutput(t, os.Getpid(), stdoutOf)
}

// ancestorOutput is the task output file the stdout of pid's nearest
// ancestor below the Claude Code CLI writes to (stdout), "" for none.
func ancestorOutput(t *proc.Table, pid int, stdout func(pid int) string) string {
	for _, p := range t.Ancestors(pid) {
		if p.Comm == claudeComm {
			return ""
		}
		if o := stdout(p.PID); strings.HasSuffix(o, taskOutputExt) {
			return o
		}
	}
	return ""
}

// stdoutOf is the file pid's stdout writes to, "" for a pipe or a socket,
// or where the platform cannot say.
func stdoutOf(pid int) string {
	p, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "fd", "1"))
	if err != nil || !filepath.IsAbs(p) {
		return ""
	}
	return p
}

// liveBinary is this process's executable; nil where /proc cannot say,
// which is never replaced.
type liveBinary struct{ b *platform.Binary }

func (l liveBinary) Path() string {
	if l.b == nil {
		return ""
	}
	return l.b.Path
}

func (l liveBinary) Replaced() bool { return l.b.Replaced() }

func (l liveBinary) Exec(env ...string) error { return l.b.Exec(env...) }

// runDetached runs spec's devctl outside its caller (launchChild) and
// follows it to its end (followChild), its files removed; started gets
// merge-child's pid. A follow cut by the caller's timeout or past leave, or
// ended with a caller going away (spec.WithCaller), leaves the files to the
// run's merge-child. A merge's gate launches and follows apart, as it
// re-executes a replaced binary in between.
func runDetached(spec ChildSpec, base string, leave time.Time, started func(pid int)) (r ChildRun) {
	defer func() {
		if !r.Left && !r.Ended {
			RemoveMergeFiles(base)
		}
	}()
	pid, err := LaunchChild(spec, base)
	if err != nil {
		GateLine("devctl does not start: %v", err)
		if filepath.Base(filepath.Dir(base)) == ownedRuns {
			// Followers may already be waiting before a PID is published.
			// Keep the known failure after the startup files are removed.
			writeResult(base, runResult{RC: guard.ExitNotFound})
		}
		return ChildRun{Rc: guard.ExitNotFound}
	}
	started(pid)
	return FollowChild(base, pid, 0, nil, nil, leave, spec.WithCaller, nil)
}

// leftToRunOn says the gate left the wait at base to run on (leftExt).
func leftToRunOn(base string) bool {
	_, err := os.Stat(base + LeftExt) //nolint:gosec // the gate's own file
	return err == nil
}

// followerGlob matches the marks of the runs that follow the run at base
// (followRun), each named after its follow-run's pid.
func followerGlob(base string) string { return base + ".follower-*" }

// callerGone says the gate of the wait at base is gone without leaving the
// run to run on, and no other run's follow-run still follows it.
func callerGone(base string, gate int) bool {
	if proc.Alive(gate) {
		return false
	}
	if leftToRunOn(base) {
		return false
	}
	marks, _ := filepath.Glob(followerGlob(base))
	for _, m := range marks {
		if pid, err := strconv.Atoi(strings.TrimPrefix(m, base+".follower-")); err == nil && proc.Alive(pid) {
			return false
		}
	}
	return true
}

func (a *app) mergeChildCmd() *cobra.Command {
	return &cobra.Command{
		Use:    MergeChildCmd + " <base>",
		Short:  "Run one gated devctl command outside its caller and hand its outcome to its owner",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.mergeChildRun(cmd.Context(), args[0], runningBinary())
		},
	}
}

// mergeChildRun runs merge-child for base: the command, then the outcome to
// its owner (tellOwner). A binary replaced while the command ran (beekeeper
// self-update, install) re-executes the installed one with the outcome
// (mergeDoneEnv), so the installed release hands it over; one that does not
// start hands it over under this one.
func (a *app) mergeChildRun(ctx context.Context, base string, bin Binary) error {
	r := mergeChild(base)
	if bin.Replaced() {
		if raw, err := json.Marshal(r.done()); err == nil {
			_, _ = fmt.Fprintf(os.Stderr, "%s%s was replaced while %s ran: re-executing it to hand the outcome over\n", GatePrefix, bin.Path(), r.command())
			err := bin.Exec(mergeDoneEnv + "=" + string(raw))
			_, _ = fmt.Fprintf(os.Stderr, "%sthe new binary does not start (%v): handing the outcome over under %s\n", GatePrefix, err, project.Version())
		}
	}
	a.recordLeft(ctx, r)
	a.tellOwner(ctx, r)
	if filepath.Base(filepath.Dir(r.base)) == ownedRuns && !proc.Alive(r.spec.Gate) {
		RemoveMergeFiles(r.base) // a gate gone leaves a wait's or queued merge's files to its run
	}
	return ExitCode(unitExit(r.rc))
}

// recordLeft records the finished run of a gate that left it to run on
// (its tool limit, its caller's timeout) and is gone, before its owner hears
// the outcome: its lane lists it no longer running the moment its devctl
// ended. A gate still running records the run itself; the watch's
// recordGone stays the backstop for a merge-child that died before this.
func (a *app) recordLeft(ctx context.Context, r childResult) {
	if proc.Alive(r.spec.Gate) || a.loadRun(r) != nil {
		return
	}
	a.recordGone(ctx)
}

// mergeDoneEnv carries a finished command's outcome across merge-child's
// re-exec of a replaced binary: a childDone as JSON.
const mergeDoneEnv = "BEEKEEPER_MERGE_DONE"

// childDone is a merge-child's outcome for the re-executed one: what
// tellOwner needs of the spec (not the command's environment), the base of
// the run's files and how the command ended.
type childDone struct {
	Argv    []string    `json:"argv"`
	Command []string    `json:"command,omitempty"`
	Owner   state.Party `json:"owner,omitzero"`
	Gate    int         `json:"gate,omitempty"`
	Config  string      `json:"config,omitempty"`
	HandCut string      `json:"handCut,omitempty"`
	Base    string      `json:"base"`
	RC      int         `json:"rc"`
	Doc     []byte      `json:"doc,omitempty"`
	Last    string      `json:"last,omitempty"`
	Kept    string      `json:"kept,omitempty"`
	Stop    int         `json:"stop,omitempty"`
	// StoppedBy is childResult's stoppedBy.
	StoppedBy state.Party `json:"stoppedBy,omitzero"`
	// CallerGone is childResult's callerGone.
	CallerGone bool `json:"callerGone,omitempty"`
}

// done is r for the re-executed merge-child.
func (r childResult) done() childDone {
	return childDone{Argv: r.spec.Argv, Command: r.spec.Command, Owner: r.spec.Owner, Gate: r.spec.Gate, Config: r.spec.Config,
		HandCut: r.spec.HandCut, Base: r.base, RC: r.rc, Doc: r.doc, Last: r.last, Kept: r.kept, Stop: int(r.stop), StoppedBy: r.stoppedBy, CallerGone: r.callerGone}
}

// result is the outcome d carries, as mergeChild returns it.
func (d childDone) result() childResult {
	return childResult{spec: ChildSpec{Argv: d.Argv, Command: d.Command, Owner: d.Owner, Gate: d.Gate, Config: d.Config, HandCut: d.HandCut},
		base: d.Base, rc: d.RC, doc: d.Doc, last: d.Last, kept: d.Kept, stop: syscall.Signal(d.Stop), stoppedBy: d.StoppedBy, callerGone: d.CallerGone}
}

// command names the command merge-child ran, the devctl command Argv stands
// for when there is one.
func (r childResult) command() string {
	argv := r.spec.Argv
	if len(r.spec.Command) > 0 {
		argv = r.spec.Command
	}
	if len(argv) == 0 {
		return devctlBinary
	}
	return strings.Join(append([]string{filepath.Base(argv[0])}, argv[1:]...), " ")
}

// doneResult is the outcome a replaced merge-child handed this one
// (mergeDoneEnv), false when there is none.
func doneResult(base string) (childResult, bool) {
	raw, ok := os.LookupEnv(mergeDoneEnv)
	if !ok {
		return childResult{}, false
	}
	_ = os.Unsetenv(mergeDoneEnv)
	var d childDone
	if err := json.Unmarshal([]byte(raw), &d); err != nil || d.Base != base {
		return childResult{}, false
	}
	return d.result(), true
}

// unitExit is the exit of merge-child's unit for its command's exit code rc.
// The outcome travels in base.rc to the gate, which hands it to the calling
// session and the event log; the unit fails only where a person must act:
// devctl's authentication (8), a kill or merge-child itself failing. devctl's
// verdicts (0-7, 9: a merge, a red or unfinished pull request, a refusal, a
// mistyped flag), the gate's own (76 queued, 77 refused, 74 unread, of a
// queued merge's gate) and a stop as asked (SIGTERM) are the calling
// session's to act on and end the unit successfully.
func unitExit(rc int) int {
	switch {
	case rc == DevctlAuth:
		return rc
	case rc >= 0 && rc <= DevctlUnconfirmed, rc == ExitGateQueued, rc == ExitGateRefused, rc == ExitGateUnread, rc == 128+int(syscall.SIGTERM):
		return 0
	}
	return rc
}

// childResult is how one merge-child run ended: its spec, the base of its
// files, devctl's exit code, document and last stderr line, the file its
// output is kept in, and the signal that stopped merge-child itself (a stop
// of its unit or process), 0 when none did.
type childResult struct {
	spec       ChildSpec
	base       string
	rc         int
	doc        []byte
	last, kept string
	stop       syscall.Signal
	// stoppedBy is who stopped the run with gate stop (stoppedExt), zero
	// when nobody did.
	stoppedBy state.Party
	// infra are the red checks that died on a transient infrastructure
	// failure, which the outcome names as such.
	infra []merge.Infra
	// callerGone says the run ended with its caller (spec.WithCaller): its
	// gate went away without leaving it to run on, so nobody waits for its
	// outcome and nobody is woken with it.
	callerGone bool
}

// mergeChild runs base.spec's command with its stdout in base.json and its
// stderr in base.log, records its own pid in base.pid and what gate runs
// shows of it in base.run before it starts it, the file keepOutput kept its output in in base.kept and the command's exit
// code (128+n for a signal) in base.rc last. SIGINT is passed on; SIGTERM
// and SIGHUP too, as they come from the service manager or a person, never
// from the gate's caller; the first one is the run's stop. A wait
// (spec.WithCaller) ends with its caller: once its gate is gone without the
// left mark (leftExt) and no other run follows it (followerGlob), devctl is
// sent SIGTERM and the run is callerGone. A merge's run that ends on a busy
// base (merge.BusyBase) runs again after a pause within spec.BusyBudget, its
// stdout replaced and its stderr appended, the exit code the last run's.
// Re-executed by a replaced merge-child whose
// command ended, it returns that outcome (doneResult) and runs nothing.
func mergeChild(base string) childResult {
	if r, ok := doneResult(base); ok {
		return r
	}
	r := childResult{base: base, rc: guard.ExitNotFound}
	raw, err := os.ReadFile(base + ".spec") //nolint:gosec // the gate's own file
	if err != nil {
		return r
	}
	_ = os.Remove(base + ".spec") //nolint:gosec // as above
	if err := json.Unmarshal(raw, &r.spec); err != nil || len(r.spec.Argv) == 0 {
		return r
	}
	spec := r.spec
	out, err := os.Create(base + ".json") //nolint:gosec // as above
	if err != nil {
		return r
	}
	defer func() { _ = out.Close() }()
	errs, err := os.Create(base + ".log") //nolint:gosec // as above
	if err != nil {
		return r
	}
	defer func() { _ = errs.Close() }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	processStarted, _ := proc.Started(os.Getpid())
	writeRunInfo(base, RunInfo{Command: r.command(), Owner: spec.Owner, Gate: spec.Gate, Started: clk.Now().UTC(), ProcessStarted: processStarted})
	if err := os.WriteFile(base+".pid", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil { //nolint:gosec // the gate's own file, named by the gate
		return r
	}
	var stop atomic.Value
	var gone atomic.Bool
	var current atomic.Pointer[os.Process]
	stopCh := make(chan struct{})
	go func() {
		for s := range sig {
			if n, ok := s.(syscall.Signal); ok && stop.CompareAndSwap(nil, n) {
				close(stopCh)
			}
			if p := current.Load(); p != nil {
				_ = p.Signal(s)
			}
		}
	}()
	rc := runSpec(base, spec, out, errs, &current, &stop, &gone)
	start := clk.Now()
	for run := 1; stop.Load() == nil && !gone.Load(); run++ {
		doc, _ := os.ReadFile(base + ".json") //nolint:gosec // as above
		if rc != 3 || !merge.BusyBase(doc) {
			break
		}
		pause, ok := merge.BusyNext(spec.BusyPauses, run, start, clk.Now(), spec.BusyBudget)
		if !ok {
			if spec.BusyBudget > 0 {
				_, _ = fmt.Fprintf(errs, "%s%s: %d devctl runs in %s, merge.busyBudget %s; nothing merged\n",
					GatePrefix, busySpent, run, clk.Since(start).Round(time.Second), spec.BusyBudget)
			}
			break
		}
		_, _ = fmt.Fprintf(errs, "%sthe base moved during every wait of devctl run %d (%s of the busy-base budget %s spent): the merge keeps its lane place, and devctl runs again in %s, updating the branch once its checks are green\n",
			GatePrefix, run, clk.Since(start).Round(time.Second), spec.BusyBudget, pause)
		select {
		case <-clk.After(pause):
		case <-stopCh:
		}
		if stop.Load() != nil {
			break
		}
		if _, err := out.Seek(0, io.SeekStart); err != nil || out.Truncate(0) != nil {
			break
		}
		if rc = runSpec(base, spec, out, errs, &current, &stop, &gone); rc == 0 {
			_, _ = fmt.Fprintf(errs, "%sdevctl run %d landed the merge on the busy base, %s of the busy-base budget %s spent\n",
				GatePrefix, run+1, clk.Since(start).Round(time.Second), spec.BusyBudget)
		}
	}
	if rc == 128+int(syscall.SIGKILL) {
		log, _ := os.ReadFile(base + ".log") //nolint:gosec // the gate's own file
		RecordMergeTermination(base, MergeKillReason(log))
	} else {
		_ = os.Remove(base + TerminationExt) //nolint:gosec // a newer completed run supersedes the old diagnostic
	}
	r.rc = rc
	r.stop, _ = stop.Load().(syscall.Signal)
	r.stoppedBy = readStopped(base)
	// A gate gone without the left mark while followers kept the run going
	// ended with its caller all the same: nobody waits for this outcome.
	r.callerGone = gone.Load() || spec.WithCaller && spec.Gate > 0 && !proc.Alive(spec.Gate) && !leftToRunOn(base)
	r.doc, _ = os.ReadFile(base + ".json") //nolint:gosec // as above
	r.last = lastLine(base + ".log")
	if r.kept = KeepOutput(base, r.doc, clk.Now()); r.kept != "" {
		_ = os.WriteFile(base+".kept", []byte(r.kept), 0o600) //nolint:gosec // as above
	}
	// Written whole or not at all: the gate reads it while it appears.
	if os.WriteFile(base+".rc.tmp", []byte(strconv.Itoa(rc)), 0o600) == nil { //nolint:gosec // as above
		_ = os.Rename(base+".rc.tmp", base+".rc") //nolint:gosec // as above
	}
	if filepath.Base(filepath.Dir(base)) == ownedRuns {
		writeResult(base, runResult{RC: rc, Doc: string(r.doc), Kept: r.kept})
	}
	return r
}

// busySpent opens merge-child's last line when a busy base's runs spent
// merge.busyBudget, which the run's reason carries on (runReason).
const busySpent = "busy-base budget spent"

// runSpec runs spec's command once with its stdout to out and its stderr to
// errs, the process in current for the signals merge-child passes on (one
// that came before it started, the run's stop, reaches it once it has), and
// returns its exit code (128+n for a signal). A wait (spec.WithCaller) ends
// with its caller: once its gate is gone without the left mark (leftExt)
// and no other run follows it (followerGlob), the command is sent SIGTERM
// and gone set.
func runSpec(base string, spec ChildSpec, out, errs *os.File, current *atomic.Pointer[os.Process], stop *atomic.Value, gone *atomic.Bool) int {
	started := clk.Now()
	skip := slices.Contains(spec.Env, github.DevctlSkipVersionCheckEnv+"=true")
	if spec.Config != "" || os.Getenv(beekeeperConfigEnv) != "" {
		skip = !guardsEnabledForConfig(spec.Config)
	}
	github.SetDevctlSkipVersionCheck(skip)
	c := exec.Command(spec.Argv[0], spec.Argv[1:]...) //nolint:gosec // the gate's command
	progress := newMergeProgress(base, spec.Argv, clk.Now().UTC())
	c.Stdout, c.Stderr, c.Env, c.Dir = out, io.MultiWriter(errs, progress), github.DevctlEnv(spec.Env, skip), spec.Dir
	if err := c.Start(); err != nil {
		_, _ = fmt.Fprintln(errs, err)
		return guard.ExitNotFound
	}
	current.Store(c.Process)
	defer current.Store(nil)
	if s, ok := stop.Load().(syscall.Signal); ok {
		_ = c.Process.Signal(s)
	}
	ended := make(chan struct{})
	if spec.WithCaller && spec.Gate > 0 {
		go func() {
			for {
				select {
				case <-ended:
					return
				case <-clk.After(FollowPoll):
				}
				if callerGone(base, spec.Gate) {
					gone.Store(true)
					_ = c.Process.Signal(syscall.SIGTERM)
					return
				}
			}
		}()
	}
	_ = c.Wait()
	close(ended)
	return childExit(c.ProcessState, errs, started)
}
