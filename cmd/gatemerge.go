package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// Seams for the tests.
var (
	pullState     = github.PullState
	devctlVersion = toolVersion
	devctlUpdate  = toolUpdate
	userSystemd   = plat.Launcher.Available
	selfExe       = os.Executable
)

// mergeFiles is the base of the merge's files (mergeBase), its directory
// created.
func (g *gateRun) mergeFiles() (string, error) {
	base := mergeBase(g.store.Dir(), g.repo, g.pr)
	return base, os.MkdirAll(filepath.Dir(base), 0o700)
}

// mergeBase is the base of a merge's files in the state directory: devctl's
// document (.json) and stderr (.log), its exit code (.rc), its runner's pid
// (.pid), the command it runs (.spec) and the file its output is kept in
// (.kept). Not the caller's pipes, which close with the caller's session.
func mergeBase(stateDir, repo string, pr int) string {
	return filepath.Join(stateDir, "merges", fmt.Sprintf("%s-%d", strings.ReplaceAll(repo, "/", "_"), pr))
}

// mergeExts are the extensions of a merge's files.
var mergeExts = []string{".json", ".log", ".rc", ".pid", ".spec", ".kept"}

// removeMergeFiles removes a merge's files.
func removeMergeFiles(base string) {
	for _, ext := range mergeExts {
		_ = os.Remove(base + ext)
	}
}

// keptOutputs is the directory under the state directory that keeps each
// finished merge run's output (keptOutput), keptFor how long.
const (
	keptOutputs = "merge-output"
	keptFor     = 7 * 24 * time.Hour
)

// keepOutput keeps a finished run's stderr and document for its owner in
// <state>/merge-output/<repo>-<n>-<UTC time>.log, as its files under merges/
// are removed with the run, and prunes the kept outputs older than keptFor.
// It returns the kept file, "" when there was nothing to keep.
func keepOutput(base string, doc []byte, now time.Time) string {
	dir := filepath.Join(filepath.Dir(filepath.Dir(base)), keptOutputs)
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // under the state directory, named by the gate
		return ""
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > keptFor {
				_ = os.Remove(filepath.Join(dir, e.Name())) //nolint:gosec // as above
			}
		}
	}
	stderr, _ := os.ReadFile(base + ".log") //nolint:gosec // the gate's own file
	if len(stderr) == 0 && len(doc) == 0 {
		return ""
	}
	path := filepath.Join(dir, filepath.Base(base)+"-"+now.UTC().Format("20060102T150405Z")+".log")
	out := append(append(stderr, []byte("--- document ---\n")...), doc...)
	if os.WriteFile(path, out, 0o600) != nil { //nolint:gosec // under the state directory, named by the gate
		return ""
	}
	return path
}

// finishedRun reads how a merge's run ended from its files once its runner
// is gone: its document and exit code, 137 (killed) without one.
func finishedRun(base string) (doc []byte, rc int) {
	doc, _ = os.ReadFile(base + ".json")  //nolint:gosec // the gate's own file
	raw, err := os.ReadFile(base + ".rc") //nolint:gosec // as above
	if err != nil {
		return doc, 128 + int(syscall.SIGKILL)
	}
	if rc, err = strconv.Atoi(strings.TrimSpace(string(raw))); err != nil {
		return doc, 128 + int(syscall.SIGKILL)
	}
	return doc, rc
}

// followPoll is how often the gate copies devctl's new stderr lines and
// looks for its exit code; childStart how long the merge's child may take to
// start.
const (
	followPoll = 200 * time.Millisecond
	childStart = 30 * time.Second
)

// childSpec is what merge-child runs: the gate's command, environment and
// working directory, in base.spec (0600: the environment carries tokens).
// Owner is the session the outcome goes to, Gate the pid of the gate that
// hands it over while its caller listens, Config the configuration a wake of
// the owner loads, Command the devctl command Argv stands for when Argv is
// beekeeper's own (a queued merge, a wait that follows another's poller).
type childSpec struct {
	Argv    []string    `json:"argv"`
	Command []string    `json:"command,omitempty"`
	Env     []string    `json:"env"`
	Dir     string      `json:"dir"`
	Owner   state.Party `json:"owner,omitzero"`
	Gate    int         `json:"gate,omitempty"`
	Config  string      `json:"config,omitempty"`
}

// childRun is how a detached run ended for its gate: devctl's document and
// exit code (128+n for a signal), the file its output is kept in, and
// whether a write to the caller's pipes failed.
type childRun struct {
	doc     []byte
	rc      int
	kept    string
	unheard bool
}

// runDetached runs spec's devctl outside its caller: a harness that
// ends the caller kills its process tree, and a session run as a unit takes
// its cgroup down with it. merge-child (this binary) runs devctl in a
// transient user service of its own (systemd-run), or where there is no
// user service manager as a child in a session of its own: its stdout goes
// to base.json, its stderr to base.log, which the gate follows onto its own
// stderr, and its exit code to base.rc. The gate writes the document to its
// stdout once devctl ended; a child gone without an exit code counts as
// killed (137). Only SIGINT, a person's Ctrl-C, reaches devctl; SIGTERM and
// SIGHUP, a caller going away, do not (outliveCaller keeps them from ending
// the gate). started gets merge-child's pid.
func runDetached(spec childSpec, base string, started func(pid int)) (r childRun) {
	defer removeMergeFiles(base)
	r.rc = guard.ExitNotFound
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	pid, err := launchChild(spec, base)
	if err != nil {
		gateLine("devctl does not start: %v", err)
		return r
	}
	started(pid)
	log, err := os.Open(base + ".log") //nolint:gosec // the gate's own file under the state directory
	if err == nil {
		defer func() { _ = log.Close() }()
	}
	said := false
	for {
		if log != nil {
			if _, err := io.Copy(os.Stderr, log); err != nil {
				r.unheard = true
			}
		}
		_, err := os.Stat(base + ".rc")
		gone := err != nil && !proc.Alive(pid)
		if gone {
			// The rc file is written before the runner exits: finishedRun reads it.
			if _, err := os.Stat(base + ".rc"); err != nil {
				gateLine("devctl's runner (pid %d) is gone without devctl's exit code", pid)
			}
		}
		if err == nil || gone {
			r.doc, r.rc = finishedRun(base)
			if raw, err := os.ReadFile(base + ".kept"); err == nil { //nolint:gosec // the gate's own file
				r.kept = string(raw)
			} else {
				r.kept = keepOutput(base, r.doc, time.Now())
			}
			if _, err := os.Stdout.Write(r.doc); err != nil { // the caller's pipe may be gone
				r.unheard = true
			}
			return r
		}
		select {
		case s := <-sig:
			switch {
			case s == syscall.SIGINT:
				if p, err := os.FindProcess(pid); err == nil {
					_ = p.Signal(os.Interrupt)
				}
			case !said:
				gateLine("%v: the caller is going away; devctl runs on outside it (pid %d) and its outcome reaches its owner", s, pid)
				said = true
			}
		case <-time.After(followPoll):
		}
	}
}

// launchChild writes spec, with this process's environment, directory and pid as
// its gate, to base.spec, starts merge-child on it and returns its pid.
func launchChild(spec childSpec, base string) (int, error) {
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		return 0, err
	}
	removeMergeFiles(base)
	path, err := exec.LookPath(spec.Argv[0])
	if err != nil {
		return 0, err
	}
	spec.Argv = append([]string{path}, spec.Argv[1:]...)
	spec.Env, spec.Gate = os.Environ(), os.Getpid()
	spec.Dir, _ = os.Getwd()
	raw, _ := json.Marshal(spec)
	if err := os.WriteFile(base+".spec", raw, 0o600); err != nil {
		return 0, err
	}
	if err := startChild(base); err != nil {
		return 0, err
	}
	return awaitPID(base)
}

// startChild starts merge-child for base: in a transient user service, else
// in a session of its own, reaped in the background.
func startChild(base string) error {
	self, err := selfExe()
	if err != nil {
		return err
	}
	if userSystemd() {
		unit := fmt.Sprintf("beekeeper-merge-%s-%d", filepath.Base(base), time.Now().UnixNano())
		return plat.Launcher.Start(platform.Unit{Name: unit, Argv: []string{self, mergeChildCmd, base}})
	}
	c := exec.Command(self, mergeChildCmd, base) //nolint:gosec // as above
	platform.Detach(c)
	if err := c.Start(); err != nil {
		return err
	}
	go func() { _ = c.Wait() }()
	return nil
}

// awaitPID waits up to childStart for merge-child to record its pid.
func awaitPID(base string) (int, error) {
	deadline := time.Now().Add(childStart)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(base + ".pid"); err == nil { //nolint:gosec // the gate's own file
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				return pid, nil
			}
		}
		time.Sleep(followPoll)
	}
	return 0, fmt.Errorf("no pid in %s.pid after %s", base, childStart)
}

// mergeChildCmd is the hidden command that runs one gated devctl command
// for the gate.
const mergeChildCmd = "merge-child"

func (a *app) mergeChildCmd() *cobra.Command {
	return &cobra.Command{
		Use:    mergeChildCmd + " <base>",
		Short:  "Run one gated devctl command outside its caller and hand its outcome to its owner",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			r := mergeChild(args[0])
			a.tellOwner(cmd.Context(), r)
			if filepath.Base(filepath.Dir(r.base)) == ownedRuns && !proc.Alive(r.spec.Gate) {
				removeMergeFiles(r.base) // a gate gone leaves a wait's or queued merge's files to its run
			}
			return exitCode(unitExit(r.rc))
		},
	}
}

// unitExit is the exit of merge-child's unit for devctl's exit code rc. The
// outcome travels in base.rc to the gate, which hands it to the calling
// session and the event log; the unit fails only where a person must act:
// devctl's usage or tooling failure (7), its authentication (8), a signal
// or merge-child itself failing. A merge, a red or unfinished pull request
// and a refusal (0-6, 9) are the calling session's to act on and end the
// unit successfully.
func unitExit(rc int) int {
	switch rc {
	case devctlUsage, devctlAuth:
		return rc
	}
	if rc >= 0 && rc <= devctlUnconfirmed {
		return 0
	}
	return rc
}

// devctl pr merge's exit codes the unit's exit tells apart.
const (
	devctlUsage       = 7
	devctlAuth        = 8
	devctlUnconfirmed = 9
)

// childResult is how one merge-child run ended: its spec, the base of its
// files, devctl's exit code, document and last stderr line, and the file its
// output is kept in.
type childResult struct {
	spec       childSpec
	base       string
	rc         int
	doc        []byte
	last, kept string
}

// mergeChild runs base.spec's command with its stdout in base.json and its
// stderr in base.log, records its own pid in base.pid before it starts it,
// the file keepOutput kept its output in in base.kept and the command's exit
// code (128+n for a signal) in base.rc last. SIGINT is passed on; SIGTERM
// and SIGHUP too, as they come from the service manager or a person, never
// from the gate's caller.
func mergeChild(base string) childResult {
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
	c := exec.Command(spec.Argv[0], spec.Argv[1:]...) //nolint:gosec // the gate's command
	c.Stdout, c.Stderr, c.Env, c.Dir = out, errs, spec.Env, spec.Dir
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	if err := os.WriteFile(base+".pid", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil { //nolint:gosec // the gate's own file, named by the gate
		return r
	}
	rc := guard.ExitNotFound
	if err := c.Start(); err == nil {
		go func() {
			for s := range sig {
				_ = c.Process.Signal(s)
			}
		}()
		_ = c.Wait()
		rc = c.ProcessState.ExitCode()
		if ws, ok := c.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			rc = 128 + int(ws.Signal())
		}
	} else {
		_, _ = fmt.Fprintln(errs, err)
	}
	r.rc = rc
	r.doc, _ = os.ReadFile(base + ".json") //nolint:gosec // as above
	r.last = lastLine(base + ".log")
	if r.kept = keepOutput(base, r.doc, time.Now()); r.kept != "" {
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
