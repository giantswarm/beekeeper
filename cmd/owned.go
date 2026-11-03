package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// ownedRuns is the directory under the state directory that holds the files
// of the running waits (ownedBase), as merges/ holds a merge's.
const ownedRuns = "runs"

// ownerWait bounds how long a run's merge-child waits for its gate to hand
// the outcome to the caller.
const ownerWait = 5 * time.Minute

// verbUnheard logs a run's outcome its caller did not hear, handed to its
// owner.
const verbUnheard = "devctl.unheard"

// gateParty logs what a run's merge-child does for its owner.
var gateParty = state.Party{Name: "beekeeper gate"}

// wakeOwner wakes an agent beekeeper owes a message, a run's owner or a
// timer's agent; a seam for the tests.
var wakeOwner = (*app).wakeAgent

// ownedRun runs one of devctl's blocking waits (pr wait, release wait,
// rollout wait) as the gate runs a merge, without a queue: outside its
// caller (runDetached), its output and exit code unchanged, its outcome
// handed to its owner by the run's merge-child once the caller no longer
// listens (tellOwner). A wait ends with its caller (childSpec.WithCaller): a
// caller stopped or gone (TaskStop, a killed shell, an ended turn) stops its
// devctl and wakes nobody. A wait whose caller's timeout cuts the call, or
// that reaches the call's limit (merge.toolLimit), runs on: the call exits
// 76, as a queued merge does, leaves the left mark (leftExt) and the outcome
// wakes the owner. At the GitHub budget's reset it starts after the merges
// queued for it (yieldToMerges).
func (a *app) ownedRun(ctx context.Context, argv []string, limit time.Duration) error {
	defer outliveCaller()()
	if err := a.yieldToMerges(ctx, argv); err != nil {
		return err
	}
	me, err := a.caller()
	if err != nil {
		me = state.Party{}
	}
	cli := callerCLI()
	base := ownedBase(a.store.Dir(), argv, os.Getpid())
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		gateLine("%v: this runs unowned, its outcome reaches nobody if your turn ends first", err)
		return exitCode(runChild(argv, os.Stdout))
	}
	spec := childSpec{Argv: argv, Owner: me, Config: a.explicitConfig(), WithCaller: true}
	lead, polls := claimLead(a.store.Dir(), argv, base)
	if !polls {
		if self, err := selfExe(); err == nil {
			spec.Argv, spec.Command = []string{self, followRunCmd, lead}, argv
			gateLine("the same command already polls GitHub on this machine (%s): following its outcome instead of polling again", filepath.Base(lead))
		}
	}
	var pid int
	r := runDetached(spec, base, leaveAt(time.Now(), limit, false), func(p int) { pid = p })
	if r.ended {
		// Its merge-child stops devctl once this gate is gone.
		return exitCode(r.rc)
	}
	if r.left {
		// The run goes on and keeps the lead: its merge-child hands the
		// outcome over once this gate is gone without its heard marker.
		if err := os.WriteFile(base+leftExt, nil, 0o600); err != nil {
			gateLine("the run cannot be marked to run on (%v): it ends with this call", err)
		}
		gateLine("%s runs on outside the call (pid %d), %s: do not run it again, do not poll", strings.Join(argv, " "), pid, ownerOf(me))
		return &exitError{code: ExitGateQueued}
	}
	if polls {
		releaseLead(a.store.Dir(), argv, base)
	}
	handOver(base, r, cli)
	return exitCode(r.rc)
}

// yieldPoll is how often a wait that yields to the merges at the reset
// reads the lanes again.
var yieldPoll = 5 * time.Second

// yieldToMerges holds a wait back while merges queued under the GitHub
// budget floor for the reset that came go first (merge.ResetFirst): it
// returns once none of them waits any more, at the end of their window
// (merge.resetFirst) at the latest, or with the context's error.
func (a *app) yieldToMerges(ctx context.Context, argv []string) error {
	said := false
	for {
		st, err := a.store.Read()
		if err != nil {
			return nil
		}
		first := merge.ResetFirst(st, time.Now(), a.cfg.Merge.ResetFirst.Duration, proc.Alive)
		if len(first) == 0 {
			return nil
		}
		if !said {
			gateLine("waiting: %s, queued for the GitHub budget's reset, goes first (merge.resetFirst); %s starts after it, at %s at the latest",
				merge.Keys(first), strings.Join(argv, " "), clock(time.Now(), merge.ResetFirstUntil(first, a.cfg.Merge.ResetFirst.Duration)))
			said = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(yieldPoll):
		}
	}
}

// ownerOf says where the outcome of a wait that runs on outside the call
// goes: a wake of its owner, on the roster or off it (tellOwner).
func ownerOf(me state.Party) string {
	if me.Session == "" && me.HostSession == "" {
		return "its outcome reaches nobody, the caller has no session"
	}
	return fmt.Sprintf("its outcome wakes %q", me.Name)
}

// resultKept is how long a finished wait's result file stays for the runs
// that followed it.
const resultKept = time.Hour

// leadFile records the run that polls for argv: one devctl wait per command
// line on the machine, every other run of it follows that one (followRun).
func leadFile(stateDir string, argv []string) string {
	sum := sha256.Sum256([]byte(strings.Join(argv[1:], "\x00")))
	return filepath.Join(stateDir, ownedRuns, "lead-"+hex.EncodeToString(sum[:8]))
}

// claimLead makes the run at base the poller for argv unless another run of
// it polls: one whose merge-child runs and has left no result, or that
// claimed the lead within childStart and has not started yet. It returns
// that run's base, false, or base, true. It prunes the result files older
// than resultKept.
func claimLead(stateDir string, argv []string, base string) (string, bool) {
	dir := filepath.Join(stateDir, ownedRuns)
	if old, err := filepath.Glob(filepath.Join(dir, "*"+resultExt)); err == nil {
		for _, f := range old {
			if info, err := os.Stat(f); err == nil && time.Since(info.ModTime()) > resultKept {
				_ = os.Remove(f)
			}
		}
	}
	lf := leadFile(stateDir, argv)
	if raw, err := os.ReadFile(lf); err == nil { //nolint:gosec // the gate's own file
		lead := string(raw)
		_, finished := readResult(lead)
		info, _ := os.Stat(lf)
		pid := readPID(lead)
		if !finished && (pid != 0 && proc.Alive(pid) || pid == 0 && info != nil && time.Since(info.ModTime()) < childStart) {
			return lead, false
		}
	}
	if os.WriteFile(lf+".tmp", []byte(base), 0o600) == nil {
		_ = os.Rename(lf+".tmp", lf)
	}
	return base, true
}

// releaseLead removes argv's lead file while it still names base.
func releaseLead(stateDir string, argv []string, base string) {
	lf := leadFile(stateDir, argv)
	if raw, err := os.ReadFile(lf); err == nil && string(raw) == base { //nolint:gosec // the gate's own file
		_ = os.Remove(lf)
	}
}

// readPID is the pid a run's merge-child recorded, 0 for none.
func readPID(base string) int {
	raw, err := os.ReadFile(base + ".pid") //nolint:gosec // the gate's own file
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return pid
}

// resultExt is the extension of the result a wait's merge-child leaves for
// the runs that follow it; it outlives the run's other files.
const resultExt = ".result"

// runResult is a finished wait's outcome for its followers.
type runResult struct {
	RC   int    `json:"rc"`
	Doc  string `json:"doc"`
	Kept string `json:"kept,omitempty"`
}

// writeResult leaves r for the runs that follow base, written whole.
func writeResult(base string, r runResult) {
	raw, _ := json.Marshal(r)
	if os.WriteFile(base+resultExt+".tmp", raw, 0o600) == nil { //nolint:gosec // the gate's own file
		_ = os.Rename(base+resultExt+".tmp", base+resultExt) //nolint:gosec // as above
	}
}

// readResult reads the result the run at base left, false while it has none.
func readResult(base string) (runResult, bool) {
	var r runResult
	raw, err := os.ReadFile(base + resultExt) //nolint:gosec // the gate's own file
	return r, err == nil && json.Unmarshal(raw, &r) == nil
}

// followRunCmd is the hidden command a run of a command another run already
// polls for runs instead of devctl.
const followRunCmd = "follow-run"

func (a *app) followRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:    followRunCmd + " <base>",
		Short:  "Follow another run's devctl wait: its stderr, document and exit code",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			return exitCode(followRun(args[0], os.Stdout, os.Stderr))
		},
	}
}

// followRun copies the stderr of the run at lead onto stderr as it grows,
// then writes the run's document to stdout and returns its exit code, 137
// when the run is gone without a result.
// It marks the lead as followed (followerGlob) while it follows, so that
// the lead runs on when its own caller goes away.
func followRun(lead string, stdout, stderr io.Writer) int {
	mark := strings.Replace(followerGlob(lead), "*", strconv.Itoa(os.Getpid()), 1)
	if os.WriteFile(mark, nil, 0o600) == nil { //nolint:gosec // the gate's own file
		defer func() { _ = os.Remove(mark) }() //nolint:gosec // as above
	}
	log, err := os.Open(lead + ".log") //nolint:gosec // the gate's own file
	if err == nil {
		defer func() { _ = log.Close() }()
	}
	for gone := 0; ; {
		if log == nil {
			if log, err = os.Open(lead + ".log"); err == nil { //nolint:gosec // as above
				defer func() { _ = log.Close() }()
			}
		}
		if log != nil {
			_, _ = io.Copy(stderr, log)
		}
		if r, ok := readResult(lead); ok {
			if log != nil {
				_, _ = io.Copy(stderr, log)
			}
			_, _ = io.WriteString(stdout, r.Doc)
			if r.Kept != "" {
				_, _ = fmt.Fprintf(stderr, "%sthe followed run's output is kept in %s\n", GatePrefix, r.Kept)
			}
			return r.RC
		}
		if pid := readPID(lead); pid != 0 && !proc.Alive(pid) {
			if gone++; gone > 5 { // its result may still be on its way
				_, _ = fmt.Fprintf(stderr, "%sthe followed run (pid %d) is gone without its outcome: run the command again\n", GatePrefix, pid)
				return 128 + int(syscall.SIGKILL)
			}
		}
		time.Sleep(followPoll)
	}
}

// unsafeName is what a file name of a run leaves out.
var unsafeName = regexp.MustCompile(`[^\w.-]+`)

// ownedBase is the base of a wait's files, named after its command's words
// (devctl's flags left out) and the gate's pid: two waits on one pull
// request are two runs.
func ownedBase(stateDir string, argv []string, pid int) string {
	var words []string
	for _, w := range argv[1:] {
		if !strings.HasPrefix(w, "-") {
			words = append(words, unsafeName.ReplaceAllString(w, "_"))
		}
	}
	name := strings.Join(words, "-")
	return filepath.Join(stateDir, ownedRuns, fmt.Sprintf("%s-%d", name[:min(len(name), 80)], pid))
}

// outliveCaller keeps the caller going away (SIGTERM, SIGHUP, its closed
// pipes' SIGPIPE) from ending the gate, so that a write to its pipes fails
// instead; the returned func undoes it.
func outliveCaller() func() {
	away := make(chan os.Signal, 1)
	signal.Notify(away, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)
	return func() { signal.Stop(away) }
}

// callerCLI is the pid of the Claude Code CLI the gate runs under, 0 for
// none (a person's terminal).
func callerCLI() int {
	t, err := plat.Machine.Processes()
	if err != nil {
		return 0
	}
	for _, p := range t.Ancestors(os.Getpid()) {
		if p.Comm == claudeComm {
			return p.PID
		}
	}
	return 0
}

// heardFile is the marker the gate with pid gate leaves for the run's
// merge-child once the run's outcome reached a caller still listening.
func heardFile(base string, gate int) string { return fmt.Sprintf("%s.%d.heard", base, gate) }

// handOver leaves the heard marker when r's outcome reached its caller: no
// write to its pipes failed and the CLI it runs under, if any, still runs.
func handOver(base string, r childRun, cli int) {
	if r.unheard || cli != 0 && !proc.Alive(cli) {
		return
	}
	_ = os.WriteFile(heardFile(base, os.Getpid()), nil, 0o600) //nolint:gosec // under the state directory, named by the gate
}

// tellOwner hands a finished run's outcome to its owner unless the run's gate
// handed it to a caller still listening: once the gate is gone without its
// heard marker (a headless turn that ended, a caller killed, its CLI gone),
// it logs devctl.unheard and wakes the owner, on the roster or off it, with the
// outcome in one line (agents wake: by name when its CLI runs, else a
// headless turn). The outcome is owed in the state first (owe), so a
// merge-child stopped or killed on the way leaves it to the watch, and a
// hand-over that fails (GitHub unanswered for a stopped merge, a wake the
// service manager refuses) stays owed for the watch's next try (payOwed).
// A run its owner stopped with gate stop wakes nobody.
func (a *app) tellOwner(ctx context.Context, r childResult) {
	if r.spec.Owner.Session == "" && r.spec.Owner.HostSession == "" {
		return
	}
	if r.callerGone {
		_, _ = fmt.Fprintf(os.Stderr, "%s ended with its caller, nobody is woken\n", r.command())
		return
	}
	if r.stoppedBy.Is(r.spec.Owner) {
		_, _ = fmt.Fprintf(os.Stderr, "%s was stopped by its owner (gate stop), nobody is woken\n", r.command())
		return
	}
	if err := a.loadRun(r); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "%s, unheard by %q: the configuration does not load (%v), nobody is woken\n", r.outcome(), r.spec.Owner.Name, err)
		return
	}
	o := r.owed(time.Now())
	if err := a.owe(o); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "%s, owed to %q: the state does not take it (%v)\n", o.Line, o.Owner.Name, err)
	}
	for deadline := time.Now().Add(ownerWait); proc.Alive(r.spec.Gate) && time.Now().Before(deadline); {
		time.Sleep(followPoll)
	}
	if heard(o) {
		a.paid(o)
		return
	}
	if merge.Red(r.doc) {
		r.infra = infraReds(ctx, r.doc)
		o.Line = r.outcome()
	}
	a.payOwed(ctx, o)
}

// loadRun loads, once, the configuration and state of the gate that
// started r: merge-child loads nothing until its command ended.
func (a *app) loadRun(r childResult) error {
	if a.store != nil {
		return nil
	}
	a.cfgPath = r.spec.Config
	return a.load()
}

// outcome is the run's outcome in one line: "<command> exit N: <reason>
// (output in <file>)", the reason the document's verdict and reason (a
// merge's release), else devctl's last stderr line. A run whose merge-child
// was stopped (stop) before its command reported a merge has no outcome: the
// line names the stop, and who stopped it with gate stop (stoppedBy), and
// that nothing waits for the command any more.
func (r childResult) outcome() string {
	o, _ := merge.ParseDocument(r.doc)
	if r.stoppedBy != (state.Party{}) && !o.Merged {
		return fmt.Sprintf("%s did not settle: %q stopped it (beekeeper gate stop, exit %d) and nothing waits for it any more: check the pull request or release it waited for and run it again if it still needs to",
			r.command(), r.stoppedBy.Name, r.rc)
	}
	if r.stop != 0 && !o.Merged {
		return fmt.Sprintf("%s did not settle: its waiter was stopped by %s (exit %d) and nothing waits for it any more: check the pull request or release it waited for and run it again if it still needs to",
			r.command(), signalName(r.stop), r.rc)
	}
	line := fmt.Sprintf("%s exit %d: %s", r.command(), r.rc, withInfra(runReason(r.doc, r.last, r.spec.HandCut), r.infra))
	if r.kept != "" {
		line += " (output in " + r.kept + ")"
	}
	return line
}

// signalName is s's name as kill(1) spells it, for the signals merge-child
// takes as its stop.
func signalName(s syscall.Signal) string {
	switch s {
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGHUP:
		return "SIGHUP"
	}
	return s.String()
}

// runReason is what a run's document says about its end (its verdict and
// reason, the failed checks the reason does not name), else its last stderr
// line, with a busy base's spent budget (busySpent) the last line names;
// handCut is a merge's base branch that no Auto-release run tags, whose
// release nobody awaits. A merge declined on classic branch
// protection is its cause and fix alone (merge.ClassicProtection), which the
// truncation of devctl's long reason would cut; a head whose workflows never
// ran (merge.NoRuns) gets what to do after it (merge.NoRunsHint), which the
// truncation never cuts either.
func runReason(doc []byte, last, handCut string) string {
	if why := merge.ClassicProtection(doc); why != "" {
		return why
	}
	var d struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	_ = json.Unmarshal(doc, &d)
	why := strings.Trim(d.Verdict+": "+d.Reason, ": ")
	var unnamed []string
	for _, c := range merge.FailedChecks(doc) {
		if !strings.Contains(why, c) {
			unnamed = append(unnamed, c)
		}
	}
	if len(unnamed) > 0 {
		why = strings.Trim(why+"; failed: "+strings.Join(unnamed, ", "), "; ")
	}
	if spent := strings.TrimPrefix(last, GatePrefix); merge.BusyBase(doc) && strings.HasPrefix(spent, busySpent) {
		why += "; " + spent
	}
	if o, ok := merge.ParseDocument(doc); ok && o.Merged {
		rel := o.Release
		switch {
		case handCut != "":
			rel = "none awaited, " + handCut + " has no auto-release and its tags are cut by hand"
		case o.NoRelease:
			rel = "none warranted"
		case rel == "":
			rel = "unknown"
		}
		why = strings.Trim("merged, release "+rel+"; "+why, "; ")
	}
	switch {
	case why != "" && merge.NoRuns(doc):
		return truncate(why, 300) + "; " + merge.NoRunsHint
	case why != "":
		return truncate(why, 300)
	case last != "":
		return last
	}
	return noOutput
}

// withInfra is a run's reason followed by the red checks that died on a
// transient infrastructure failure (merge.InfraNote), each with its rerun:
// past the reason's truncation, so the rerun survives it.
func withInfra(reason string, infra []merge.Infra) string {
	if note := merge.InfraNote(infra); note != "" {
		return strings.Trim(reason+"; "+note, "; ")
	}
	return reason
}

// noOutput is runReason for a run that said nothing.
const noOutput = "no output"

// runFailure is a gated run's reason for its record, "" when it said nothing.
func runFailure(doc []byte, last, handCut string) string {
	if why := runReason(doc, last, handCut); why != noOutput {
		return why
	}
	return ""
}

// lastLine is the last non-empty line of the file at path, shortened, ""
// when there is none.
func lastLine(path string) string {
	raw, err := os.ReadFile(path) //nolint:gosec // the gate's own file
	if err != nil {
		return ""
	}
	return lastOf(string(raw))
}

// lastOf is the last non-empty line of s, shortened.
func lastOf(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return truncate(lines[len(lines)-1], 200)
}
