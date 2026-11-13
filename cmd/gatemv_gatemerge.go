package cmd

import (
	"bytes"
	"cmp"
	"context"
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

	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

var pullState = github.PullState

var pullBase = func(ctx context.Context, repo string, n int) (github.PullBase, error) {
	return github.ReadPullBase(ctx, github.RunGH, repo, n)
}

var releaseAt = func(ctx context.Context, repo, base, ref string) (github.BaseRelease, error) {
	return github.ReadReleaseAt(ctx, github.RunGH, repo, base, ref)
}

var pluginTags = func(ctx context.Context, repo, sha string) ([]string, error) {
	return github.PluginTags(ctx, github.RunGH, repo, sha)
}

var tagExists = func(ctx context.Context, repo, tag string) (bool, error) {
	return github.TagExists(ctx, github.RunGH, repo, tag)
}

// releaseWait runs devctl release wait for repo's release named by
// target (a version, or --pr and a number), its progress on the gate's
// stderr, and returns its document.
var releaseWait = func(ctx context.Context, repo string, target ...string) []byte {
	var out bytes.Buffer
	c := exec.CommandContext(ctx, merge.Tool, append([]string{"release", "wait", repo}, target...)...) //nolint:gosec // the merge tool, a checked repository and release
	c.Env = devctlCommandEnv("")
	c.Stdout, c.Stderr = &out, os.Stderr
	_ = c.Run() // its exit code is the document's verdict
	return out.Bytes()
}

// releasePRWait bounds the wait for a release pull request's tag
// pipeline: devctl release wait's own default timeout and a margin.
var releasePRWait = 35 * time.Minute

// pluginTagPoll is how often the gate looks for a merged plugin's tag,
// pluginTagWait how long it awaits the tags of one merge.
var pluginTagPoll = 10 * time.Second

var pluginTagWait = 5 * time.Minute

var pullRebase = func(ctx context.Context, repo string, n int) (github.Rebase, error) {
	return github.ReadRebase(ctx, github.RunGH, repo, n)
}

var pullMergeCommits = func(ctx context.Context, repo string, n int) ([]string, error) {
	return github.MergeCommits(ctx, github.RunGH, repo, n)
}

// reviewGuard is the pull request's review read (github.ReviewGuard).
var reviewGuard = func(ctx context.Context, repo string, n int) (string, error) {
	why, _, err := github.ReviewGuard(ctx, github.RunGH, repo, n)
	return why, err
}

// prWarmWait runs devctl pr wait for repo#n, its progress on the gate's
// stderr: a warm-up whose verdict the merge's own devctl run judges.
var prWarmWait = func(ctx context.Context, repo string, n int) {
	c := exec.CommandContext(ctx, merge.Tool, "pr", "wait", repo, strconv.Itoa(n)) //nolint:gosec // the merge tool, a checked repository and number
	c.Env = devctlCommandEnv("")
	c.Stdout, c.Stderr = io.Discard, os.Stderr
	_ = c.Run()
}

// rebaseRetry is how long the gate waits for GitHub to compute whether a
// pull request rebases before it reads again.
var rebaseRetry = 2 * time.Second

var devctlVersion = toolVersion

var baseChecks = func(ctx context.Context, repo, base string) (github.BaseChecks, error) {
	return github.ReadBaseChecks(ctx, github.RunGH, repo, base)
}

// infraReds are the red checks of a document that died on a transient
// infrastructure failure, read from their jobs' logs.
var infraReds = func(ctx context.Context, doc []byte) []merge.Infra {
	return merge.InfraReds(ctx, github.RunGH, doc)
}

var devctlAcceptsRed = toolAcceptsRed

var selfExe = os.Executable

// binary is the executable a gate call or a merge-child runs: whether
// another file was renamed over its path since it started (beekeeper
// self-update, install) and its re-exec, which carries the call on under the
// installed release with the same pid, arguments and stdio.
type binary interface {
	Path() string
	Replaced() bool
	Exec(env ...string) error
}

// mergeFiles is the base of the merge's files (mergeBase), its directory
// created.
func (g *gateRun) mergeFiles() (string, error) {
	base := mergeBase(g.a.store.Dir(), g.repo, g.pr)
	return base, os.MkdirAll(filepath.Dir(base), 0o700)
}

// mergeBase is the base of a merge's files in the state directory: devctl's
// document (.json) and stderr (.log), its exit code (.rc), its runner's pid
// (.pid), the command it runs (.spec), what gate runs shows of it (.run),
// the file its output is kept in (.kept), who stopped it with gate stop
// (.stopped) and, for a wait, the gate's mark that it left the run to run on
// (.left). A termination diagnostic (.termination) survives cleanup for the
// next invocation. Not the caller's pipes, which close with the caller's session.
func mergeBase(stateDir, repo string, pr int) string {
	return filepath.Join(stateDir, "merges", fmt.Sprintf("%s-%d", strings.ReplaceAll(repo, "/", "_"), pr))
}

// mergeExts are the extensions of a merge's files.
var mergeExts = []string{".progress.tmp", ".progress", ".json", ".log", ".rc", ".pid", ".spec", runInfoExt, ".kept", stoppedExt, leftExt}

// removeMergeFiles removes a merge's files.
func removeMergeFiles(base string) {
	root, err := os.OpenRoot(filepath.Dir(base))
	if err != nil {
		return
	}
	defer root.Close() //nolint:errcheck // best-effort cleanup
	for _, ext := range mergeExts {
		_ = root.Remove(filepath.Base(base) + ext)
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
	header := keepOutputHeader(base, doc)
	path := filepath.Join(dir, filepath.Base(base)+"-"+now.UTC().Format("20060102T150405Z")+".log")
	out := append(append(append(header, stderr...), []byte("--- document ---\n")...), doc...)
	if os.WriteFile(path, out, 0o600) != nil { //nolint:gosec // under the state directory, named by the gate
		return ""
	}
	return path
}

func keepOutputHeader(base string, doc []byte) []byte {
	var requestedBy string
	if len(doc) > 0 {
		var meta struct {
			RequestedBy string `json:"requestedBy"`
		}
		if json.Unmarshal(doc, &meta) == nil && meta.RequestedBy != "" {
			requestedBy = meta.RequestedBy
		}
	}
	info, ok := readRunInfo(base)
	var command string
	if ok && info.Command != "" {
		command = "command: " + info.Command + "\n"
	}
	owner := requestedBy
	if owner == "" && ok {
		owner = cmp.Or(info.Owner.Name, info.Owner.Session, info.Owner.HostSession)
	}
	if owner == "" {
		return []byte(command)
	}
	var details []string
	if ok {
		if s := cmp.Or(info.Owner.Session, info.Owner.HostSession); s != "" && s != owner {
			details = append(details, "session "+s)
		}
		if info.Owner.Host != "" {
			details = append(details, "host "+info.Owner.Host)
		}
		if info.Gate > 0 {
			details = append(details, fmt.Sprintf("gate pid %d", info.Gate))
		}
	}
	if len(details) > 0 {
		return []byte(command + fmt.Sprintf("requested by: %s (%s)\n", owner, strings.Join(details, ", ")))
	}
	return []byte(command + fmt.Sprintf("requested by: %s\n", owner))
}

// finishedRun reads how a merge's run ended from its files once its runner
// is gone: its document and exit code, 137 (killed) without one.
func finishedRun(base string) (doc []byte, rc int) {
	doc, _ = os.ReadFile(base + ".json")  //nolint:gosec // the gate's own file
	raw, err := os.ReadFile(base + ".rc") //nolint:gosec // as above
	if err != nil {
		recordLostMergeRunner(base)
		return doc, 128 + int(syscall.SIGKILL)
	}
	if rc, err = strconv.Atoi(strings.TrimSpace(string(raw))); err != nil {
		recordLostMergeRunner(base)
		return doc, 128 + int(syscall.SIGKILL)
	}
	if rc == 128+int(syscall.SIGKILL) {
		if _, err := os.Stat(base + terminationExt); err != nil {
			log, _ := os.ReadFile(base + ".log") //nolint:gosec // the gate's own file
			recordMergeTermination(base, mergeKillReason(log))
		}
	}
	return doc, rc
}

// terminationExt survives merge file cleanup so a later invocation can explain
// the previous failed run even when its runner could not write an exit record.
const terminationExt = ".termination"

func recordLostMergeRunner(base string) {
	log, _ := os.ReadFile(base + ".log") //nolint:gosec // the gate's own file
	reason := mergeKillReason(log)
	if strings.Contains(reason, "no cap kill confirmed") {
		reason = "runner disappeared without a valid exit record (exit 137): session end, reboot or external SIGKILL possible; no cap kill confirmed"
	}
	recordMergeTermination(base, reason)
}

func recordMergeTermination(base, reason string) {
	line := clk.Now().UTC().Format(time.RFC3339) + " " + reason + "\n"
	if err := os.WriteFile(base+terminationExt, []byte(line), 0o600); err != nil { //nolint:gosec // the gate's own state file
		gateLine("cannot keep merge termination reason: %v", err)
	}
	f, err := os.OpenFile(base+".log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // as above
	if err == nil {
		_, _ = fmt.Fprint(f, GatePrefix+line)
		_ = f.Close()
	}
	gateLine("%s", strings.TrimSpace(line))
}

// mergeKillReason preserves the wrapper's confirmed cap report when present.
// SIGKILL alone cannot identify its sender and must never be called an OOM.
func mergeKillReason(log []byte) string {
	for _, line := range strings.Split(string(log), "\n") {
		if strings.HasPrefix(line, guard.LogPrefix) && strings.Contains(line, "cap") && strings.Contains(line, "killed") {
			return "exit 137: " + strings.TrimPrefix(line, guard.LogPrefix)
		}
	}
	return "SIGKILL (exit 137): external signal or session end possible; no cap kill confirmed (OOM evidence may be unavailable)"
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
// beekeeper's own (a queued merge, a wait that follows another's poller),
// HandCut a merge's base branch that no Auto-release run tags. WithCaller
// says the run ends with its gate (a wait): merge-child stops the command
// once the gate is gone without leaving the run to run on (leftExt).
type childSpec struct {
	Argv    []string    `json:"argv"`
	Command []string    `json:"command,omitempty"`
	Env     []string    `json:"env"`
	Dir     string      `json:"dir"`
	Owner   state.Party `json:"owner,omitzero"`
	Gate    int         `json:"gate,omitempty"`
	Config  string      `json:"config,omitempty"`
	HandCut string      `json:"handCut,omitempty"`
	// WithCaller is set for a wait, never for a merge or a queued merge's run.
	WithCaller bool `json:"withCaller,omitempty"`
	// BusyBudget, set for a merge with --update-branch, is how long from its
	// first run merge-child runs devctl again after a run ends on a busy base
	// (merge.BusyBase), BusyPauses its pauses before them (merge.BusyNext);
	// zero: once.
	BusyBudget time.Duration   `json:"busyBudget,omitempty"`
	BusyPauses []time.Duration `json:"busyPauses,omitempty"`
}

// childRun is how a detached run ended for its gate: devctl's document and
// exit code (128+n for a signal), the file its output is kept in, and
// whether a write to the caller's pipes failed. replaced says the gate's
// binary was replaced while the run went on, which runs on: offset is how
// far its stderr was copied, for the re-executed gate to follow it from.
type childRun struct {
	doc      []byte
	rc       int
	kept     string
	unheard  bool
	replaced bool
	offset   int64
	// held says the document was kept off the gate's stdout (followChild's
	// hold): a run the gate may send again, whose caller reads one document.
	held bool
	// left says the follow reached its leave time while the run went on;
	// cut that it left as its caller's timeout cut the connection
	// (timedCaller).
	left, cut bool
	// ended says the follow returned as its caller went away, for a run that
	// ends with it (withCaller): its merge-child stops the command.
	ended bool
	// stopped says a SIGTERM aimed at the gate stopped the command: the gate
	// ends with it and leaves its release to lanes settle --release.
	stopped bool
}

// emit writes a held document to the gate's stdout.
func (r childRun) emit() childRun {
	if r.held {
		r.held = false
		if _, err := os.Stdout.Write(r.doc); err != nil {
			r.unheard = true
		}
	}
	return r
}

// followChild follows the devctl that merge-child pid runs outside its
// caller: a harness that ends the caller kills its process tree, and a
// session run as a unit takes its cgroup down with it. merge-child (this
// binary) runs devctl in a transient user service of its own (systemd-run),
// or where there is no user service manager as a child in a session of its
// own: its stdout goes to base.json, its stderr to base.log, which the gate
// copies onto its own stderr from offset on, and its exit code to base.rc.
// The gate writes the document to its stdout once devctl ended; a child
// gone without an exit code counts as killed (137). SIGINT, a person's
// Ctrl-C, reaches devctl at once. SIGTERM reaches it when it is a stop aimed
// at the gate: its caller still there stopGrace later. A caller going away
// (SIGHUP, or the gate's parent gone with the SIGTERM, a harness killing its
// command's process tree) does not stop devctl (outliveCaller keeps it from
// ending the gate), unless withCaller: then the follow returns at once with
// ended set, and the run's merge-child stops devctl as the gate is gone. A SIGTERM from a timeout the gate runs under
// (timedCaller) cuts the connection, not the run: the follow returns at once
// with left and cut set, the run's files in place for its merge-child, which
// hands the outcome to its owner. bin, when given, is the gate's binary: replaced while
// the run goes on, the follow returns at once with replaced set and the
// offset reached, the run's files in place, for the gate to re-execute the
// installed binary and follow on from there. hold, when given, keeps a
// document it takes off the stdout (held), for the gate to decide. leave,
// when set, is when the follow returns with left set while the run goes on
// (the gate's tool limit), its files in place for its merge-child and the
// watch. sig, when given, hears the signals from before devctl started
// (gateRun.followSignals); else the follow hears them from its start on.
func followChild(base string, pid int, offset int64, bin binary, hold func(doc []byte) bool, leave time.Time, withCaller bool, sig <-chan os.Signal) (r childRun) {
	r.rc = guard.ExitNotFound
	if sig == nil {
		own := make(chan os.Signal, 1)
		signal.Notify(own, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		defer signal.Stop(own)
		sig = own
	}
	log, err := os.Open(base + ".log") //nolint:gosec // the gate's own file under the state directory
	if err == nil {
		defer func() { _ = log.Close() }()
		if _, err := log.Seek(offset, io.SeekStart); err != nil {
			gateLine("devctl's output so far cannot be skipped (%v): it is copied once more", err)
		}
	}
	said, ppid, term := false, os.Getppid(), time.Time{}
	away := func(s os.Signal) bool {
		if withCaller {
			gateLine("%v: the caller is going away, and the wait ends with it (pid %d)", s, pid)
			r.rc, r.ended = 128+int(syscall.SIGTERM), true
			return true
		}
		if !said {
			gateLine("%v: the caller is going away; devctl runs on outside it (pid %d) and its outcome reaches its owner", s, pid)
			said = true
		}
		return false
	}
	for {
		if log != nil {
			if _, err := io.Copy(os.Stderr, log); err != nil {
				r.unheard = true
			}
		}
		if bin != nil && bin.Replaced() {
			r.replaced = true
			if log != nil {
				r.offset, _ = log.Seek(0, io.SeekCurrent)
			}
			return r
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
				r.kept = keepOutput(base, r.doc, clk.Now())
			}
			if r.held = hold != nil && hold(r.doc); r.held {
				return r
			}
			if _, err := os.Stdout.Write(r.doc); err != nil { // the caller's pipe may be gone
				r.unheard = true
			}
			return r
		}
		if !leave.IsZero() && !clk.Now().Before(leave) {
			r.left = true
			return r
		}
		select {
		case s := <-sig:
			switch s {
			case syscall.SIGINT:
				signalPID(pid, os.Interrupt)
			case syscall.SIGHUP:
				if away(s) {
					return r
				}
			case syscall.SIGTERM:
				if !said && timedCaller() {
					gateLine("SIGTERM from the caller's timeout: it cut the connection, not the wait; devctl runs on outside the call (pid %d)", pid)
					r.left, r.cut = true, true
					return r
				}
				if term.IsZero() && !said {
					term = clk.Now()
				}
			}
		case <-clk.After(followPoll):
		}
		switch {
		case term.IsZero():
		case said || os.Getppid() != ppid:
			if away(syscall.SIGTERM) {
				return r
			}
			term = time.Time{}
		case clk.Since(term) >= stopGrace:
			gateLine("SIGTERM with the caller still there: stopping devctl (pid %d); the run is recorded as one ended by a signal", pid)
			signalPID(pid, syscall.SIGTERM)
			term, said, r.stopped = time.Time{}, true, true
		}
	}
}

// timedCaller says whether the gate runs under a timeout (coreutils'
// timeout between it and the Claude Code CLI it runs under, if any), whose
// SIGTERM is the caller giving up on the call, never a stop aimed at the run;
// a seam for the tests.
var timedCaller = func() bool {
	t, err := plat.Machine.Processes()
	return err == nil && underTimeout(t, os.Getpid())
}

// timeoutComm is coreutils' timeout as the process table names it.
const timeoutComm = "timeout"

// underTimeout says whether a timeout is among pid's ancestors below the
// Claude Code CLI, if any.
func underTimeout(t *proc.Table, pid int) bool {
	for _, p := range t.Ancestors(pid) {
		switch p.Comm {
		case timeoutComm:
			return true
		case claudeComm:
			return false
		}
	}
	return false
}

// signalPID sends s to pid.
func signalPID(pid int, s os.Signal) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Signal(s)
	}
}

// stopGrace is how long a SIGTERM waits for its caller to go away before
// it counts as a stop aimed at the gate.
const stopGrace = 2 * time.Second

// launchChild writes spec, with this process's environment, directory and pid as
// its gate, to base.spec, starts merge-child on it and returns its pid.
func launchChild(spec childSpec, base string) (int, error) {
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		return 0, err
	}
	if _, err := os.Stat(base + ".pid"); err == nil {
		if raw, err := os.ReadFile(base + ".pid"); err == nil { //nolint:gosec // a path of this tool's own state
			pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
			if pid > 0 && !proc.Alive(pid) {
				_, _ = finishedRun(base)
			}
		}
	}
	if raw, err := os.ReadFile(base + terminationExt); err == nil { //nolint:gosec // a path of this tool's own state
		gateLine("previous merge termination: %s", strings.TrimSpace(string(raw)))
	}
	removeMergeFiles(base)
	path, err := exec.LookPath(spec.Argv[0])
	if err != nil {
		return 0, err
	}
	spec.Argv = append([]string{path}, spec.Argv[1:]...)
	spec.Env, spec.Gate = devctlCommandEnv(spec.Config), os.Getpid()
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
		unit := fmt.Sprintf("beekeeper-merge-%s-%d", filepath.Base(base), clk.Now().UnixNano())
		return plat.Launcher.Start(platform.Unit{Name: unit, Argv: []string{self, mergeChildCmd, base}, TermIsSuccess: true})
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
	deadline := clk.Now().Add(childStart)
	for clk.Now().Before(deadline) {
		if raw, err := os.ReadFile(base + ".pid"); err == nil { //nolint:gosec // the gate's own file
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				return pid, nil
			}
		}
		clk.Sleep(followPoll)
	}
	return 0, fmt.Errorf("no pid in %s.pid after %s", base, childStart)
}

// leftExt marks a wait its gate left to run on outside the call (its
// caller's timeout, the tool limit): its merge-child goes on without the
// gate and hands the outcome to the owner.
const leftExt = ".left"

// mergeChildCmd is the hidden command that runs one gated devctl command
// for the gate.
const mergeChildCmd = "merge-child"

// devctl pr merge's exit codes the unit's exit tells apart.
const (
	devctlReleaseFailed = 6
	devctlUsage         = 7
	devctlAuth          = 8
	devctlUnconfirmed   = 9
)
