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
	"github.com/giantswarm/beekeeper/internal/proc"
)

// Seams for the tests.
var (
	pullState     = github.PullState
	devctlVersion = toolVersion
	userSystemd   = guard.UserSystemd
)

// mergeFiles is the base of the merge's files (mergeBase), its directory
// created.
func (g *gateRun) mergeFiles() (string, error) {
	base := mergeBase(g.store.Dir(), g.repo, g.pr)
	return base, os.MkdirAll(filepath.Dir(base), 0o700)
}

// mergeBase is the base of a merge's files in the state directory: devctl's
// document (.json) and stderr (.log), its exit code (.rc), its runner's pid
// (.pid) and the command it runs (.spec). Not the caller's pipes, which close
// with the caller's session.
func mergeBase(stateDir, repo string, pr int) string {
	return filepath.Join(stateDir, "merges", fmt.Sprintf("%s-%d", strings.ReplaceAll(repo, "/", "_"), pr))
}

// mergeExts are the extensions of a merge's files.
var mergeExts = []string{".json", ".log", ".rc", ".pid", ".spec"}

// removeMergeFiles removes a merge's files.
func removeMergeFiles(base string) {
	for _, ext := range mergeExts {
		_ = os.Remove(base + ext)
	}
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
type childSpec struct {
	Argv []string `json:"argv"`
	Env  []string `json:"env"`
	Dir  string   `json:"dir"`
}

// runDetached runs the merge's devctl outside its caller: a harness that
// ends the caller kills its process tree, and a session run as a unit takes
// its cgroup down with it. merge-child (this binary) runs devctl in a
// transient user service of its own (systemd-run), or where there is no
// user service manager as a child in a session of its own: its stdout goes
// to base.json, its stderr to base.log, which the gate follows onto its own
// stderr, and its exit code to base.rc. The gate returns the document once
// devctl ended; a child gone without an exit code counts as killed (137).
// Only SIGINT, a person's Ctrl-C, reaches devctl; SIGTERM and SIGHUP, a
// caller going away, do not (runMerge keeps them from ending the gate).
// started gets merge-child's pid. rc is devctl's exit code, 128+n for a
// signal.
func runDetached(argv []string, base string, started func(pid int)) (doc []byte, rc int) {
	removeMergeFiles(base)
	defer removeMergeFiles(base)
	path, err := exec.LookPath(argv[0])
	if err != nil {
		gateLine("%v", err)
		return nil, guard.ExitNotFound
	}
	dir, _ := os.Getwd()
	spec, _ := json.Marshal(childSpec{Argv: append([]string{path}, argv[1:]...), Env: os.Environ(), Dir: dir})
	if err := os.WriteFile(base+".spec", spec, 0o600); err != nil {
		gateLine("%v", err)
		return nil, guard.ExitNotFound
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	if err := startChild(base); err != nil {
		gateLine("the merge does not start: %v", err)
		return nil, guard.ExitNotFound
	}
	pid, err := awaitPID(base)
	if err != nil {
		gateLine("the merge does not start: %v", err)
		return nil, guard.ExitNotFound
	}
	started(pid)
	log, err := os.Open(base + ".log") //nolint:gosec // the gate's own file under the state directory
	if err == nil {
		defer func() { _ = log.Close() }()
	}
	said := false
	for {
		if log != nil {
			_, _ = io.Copy(os.Stderr, log)
		}
		if _, err := os.Stat(base + ".rc"); err == nil {
			doc, rc = finishedRun(base)
			return doc, rc
		}
		if !proc.Alive(pid) {
			// The rc file is written before the runner exits: finishedRun reads it.
			if _, err := os.Stat(base + ".rc"); err != nil {
				gateLine("the merge's runner (pid %d) is gone without devctl's exit code", pid)
			}
			doc, rc = finishedRun(base)
			return doc, rc
		}
		select {
		case s := <-sig:
			switch {
			case s == syscall.SIGINT:
				if p, err := os.FindProcess(pid); err == nil {
					_ = p.Signal(os.Interrupt)
				}
			case !said:
				gateLine("%v: the caller is going away; devctl merges on outside it (pid %d) and the gate records its outcome", s, pid)
				said = true
			}
		case <-time.After(followPoll):
		}
	}
}

// startChild starts merge-child for base: in a transient user service, else
// in a session of its own, reaped in the background.
func startChild(base string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if userSystemd() {
		unit := fmt.Sprintf("beekeeper-merge-%s-%d", filepath.Base(base), time.Now().UnixNano())
		out, err := exec.Command("systemd-run", "--user", "--collect", "--quiet", "--unit="+unit, //nolint:gosec // this binary and its own file
			"-p", "KillMode=mixed", "--", self, mergeChildCmd, base).CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemd-run: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	c := exec.Command(self, mergeChildCmd, base) //nolint:gosec // as above
	detach(c)
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

// mergeChildCmd is the hidden command that runs one merge's devctl for the
// gate.
const mergeChildCmd = "merge-child"

func (a *app) mergeChildCmd() *cobra.Command {
	return &cobra.Command{
		Use:    mergeChildCmd + " <base>",
		Short:  "Run one gated merge's devctl outside its caller",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			return exitCode(mergeChild(args[0]))
		},
	}
}

// mergeChild runs base.spec's command with its stdout in base.json and its
// stderr in base.log, records its own pid in base.pid before it starts it and the
// command's exit code (128+n for a signal) in base.rc last. SIGINT is passed
// on; SIGTERM and SIGHUP too, as they come from the service manager or a
// person, never from the gate's caller.
func mergeChild(base string) int {
	raw, err := os.ReadFile(base + ".spec") //nolint:gosec // the gate's own file
	if err != nil {
		return guard.ExitNotFound
	}
	_ = os.Remove(base + ".spec") //nolint:gosec // as above
	var spec childSpec
	if err := json.Unmarshal(raw, &spec); err != nil || len(spec.Argv) == 0 {
		return guard.ExitNotFound
	}
	out, err := os.Create(base + ".json") //nolint:gosec // as above
	if err != nil {
		return guard.ExitNotFound
	}
	defer func() { _ = out.Close() }()
	errs, err := os.Create(base + ".log") //nolint:gosec // as above
	if err != nil {
		return guard.ExitNotFound
	}
	defer func() { _ = errs.Close() }()
	c := exec.Command(spec.Argv[0], spec.Argv[1:]...) //nolint:gosec // the gate's command
	c.Stdout, c.Stderr, c.Env, c.Dir = out, errs, spec.Env, spec.Dir
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	if err := os.WriteFile(base+".pid", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil { //nolint:gosec // the gate's own file, named by the gate
		return guard.ExitNotFound
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
	// Written whole or not at all: the gate reads it while it appears.
	if os.WriteFile(base+".rc.tmp", []byte(strconv.Itoa(rc)), 0o600) == nil { //nolint:gosec // as above
		_ = os.Rename(base+".rc.tmp", base+".rc") //nolint:gosec // as above
	}
	return rc
}
