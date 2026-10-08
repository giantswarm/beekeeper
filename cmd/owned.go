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
	"slices"
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

// gateParty logs what a run's merge-child does for its owner.
var gateParty = state.Party{Name: "beekeeper gate"}

// wakeOwner wakes an agent beekeeper owes a message, a run's owner or a
// timer's agent; a seam for the tests.
var wakeOwner = (*app).wakeAgent

// ownedRun runs one of devctl's blocking waits (pr wait, release wait,
// rollout wait) as the gate runs a merge, without a queue: outside its
// caller (runDetached), its output and exit code unchanged, its outcome
// handed to its owner by the run's merge-child once the caller no longer
// listens (tellOwner).
func (a *app) ownedRun(argv []string) error {
	defer outliveCaller()()
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
	spec := childSpec{Argv: argv, Owner: me, Config: a.explicitConfig()}
	if lead, ok := claimLead(a.store.Dir(), argv, base); ok {
		defer releaseLead(a.store.Dir(), argv, base)
	} else if self, err := selfExe(); err == nil {
		spec.Argv, spec.Command = []string{self, followRunCmd, lead}, argv
		gateLine("the same command already polls GitHub on this machine (%s): following its outcome instead of polling again", filepath.Base(lead))
	}
	r := runDetached(spec, base, func(int) {})
	handOver(base, r, cli)
	return exitCode(r.rc)
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
func followRun(lead string, stdout, stderr io.Writer) int {
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
// it logs devctl.unheard and wakes the owner, a registered agent, with the
// outcome in one line (agents wake: by name when its CLI runs, else a
// headless turn).
func (a *app) tellOwner(ctx context.Context, r childResult) {
	if r.spec.Owner.Session == "" && r.spec.Owner.HostSession == "" {
		return
	}
	for deadline := time.Now().Add(ownerWait); proc.Alive(r.spec.Gate) && time.Now().Before(deadline); {
		time.Sleep(followPoll)
	}
	heard := heardFile(r.base, r.spec.Gate)
	_, err := os.Stat(heard)
	_ = os.Remove(heard)
	if err == nil {
		return
	}
	line := r.outcome()
	if a.store == nil {
		a.cfgPath = r.spec.Config
		if err := a.load(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "%s, unheard by %q: the configuration does not load (%v), nobody is woken\n", line, r.spec.Owner.Name, err)
			return
		}
	}
	st, err := a.store.Read()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "%s, unheard by %q: the state does not load (%v), nobody is woken\n", line, r.spec.Owner.Name, err)
		return
	}
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(r.spec.Owner) })
	if i < 0 {
		_ = a.store.Log(event(gateParty, "devctl.unheard", "%s; %q is no registered agent, nobody is woken", line, r.spec.Owner.Name))
		return
	}
	ag := st.Agents[i]
	_ = a.store.Log(event(gateParty, "devctl.unheard", "%s; waking %q", line, ag.Name))
	q := ag.Session
	if q == "" {
		q = ag.HostSession
	}
	if err := wakeOwner(a, ctx, gateParty, q, line, ""); err != nil {
		_ = a.store.Log(event(gateParty, "devctl.unheard", "waking %q failed (%v): %s", ag.Name, err, line))
	}
}

// outcome is the run's outcome in one line: "<command> exit N: <reason>
// (output in <file>)", the reason the document's verdict and reason (a
// merge's release), else devctl's last stderr line.
func (r childResult) outcome() string {
	argv := r.spec.Argv
	if len(r.spec.Command) > 0 {
		argv = r.spec.Command
	}
	if len(argv) == 0 {
		argv = []string{"devctl"}
	}
	cmd := strings.Join(append([]string{filepath.Base(argv[0])}, argv[1:]...), " ")
	line := fmt.Sprintf("%s exit %d: %s", cmd, r.rc, runReason(r.doc, r.last, r.spec.HandCut))
	if r.kept != "" {
		line += " (output in " + r.kept + ")"
	}
	return line
}

// runReason is what a run's document says about its end, else its last
// stderr line; handCut is a merge's base branch that no Auto-release run
// tags, whose release nobody awaits.
func runReason(doc []byte, last, handCut string) string {
	var d struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	_ = json.Unmarshal(doc, &d)
	why := strings.Trim(d.Verdict+": "+d.Reason, ": ")
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
	case why != "":
		return truncate(why, 300)
	case last != "":
		return last
	}
	return "no output"
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
