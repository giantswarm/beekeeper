package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

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

// wakeOwner wakes a run's owner; a seam for the tests.
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
	r := runDetached(childSpec{Argv: argv, Owner: me, Config: a.explicitConfig()}, base, func(int) {})
	handOver(base, r, cli)
	return exitCode(r.rc)
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
	if i := slices.Index(argv, "--"); i >= 0 && slices.Contains(argv[:i], "gate") {
		argv = argv[i+1:] // a queued merge's run: the devctl command it gates
	}
	if len(argv) == 0 {
		argv = []string{"devctl"}
	}
	cmd := strings.Join(append([]string{filepath.Base(argv[0])}, argv[1:]...), " ")
	line := fmt.Sprintf("%s exit %d: %s", cmd, r.rc, runReason(r.doc, r.last))
	if r.kept != "" {
		line += " (output in " + r.kept + ")"
	}
	return line
}

// runReason is what a run's document says about its end, else its last
// stderr line.
func runReason(doc []byte, last string) string {
	var d struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	_ = json.Unmarshal(doc, &d)
	why := strings.Trim(d.Verdict+": "+d.Reason, ": ")
	if o, ok := merge.ParseDocument(doc); ok && o.Merged {
		rel := o.Release
		switch {
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
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	return truncate(lines[len(lines)-1], 200)
}
