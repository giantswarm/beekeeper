package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// restartFile is the request an install leaves in the state directory: each
// long-running process of the binary it replaced (a watch, the standby
// unit's included, and a waiting reopen) restarts on the installed one once,
// at its next quiet moment.
const restartFile = "restart.json"

// restartedEnv carries the request a re-executed process restarted for, so
// it restarts for each request once.
const restartedEnv = "BEEKEEPER_RESTARTED"

// The names of the commands a watch runs under: watch, alerts watch, guide
// watch.
const (
	watchName  = "watch"
	alertsName = "alerts"
	guideName  = "guide"
)

// errRestart ends a run that restarts on the installed binary.
var errRestart = errors.New("restarting on the installed binary")

// restartRequest is the restart file's content.
type restartRequest struct {
	At time.Time `json:"at"`
	// Binary is the path the install wrote.
	Binary string `json:"binary"`
}

// restarts reports whether p is a process an install restarts: a running
// beekeeper watch (not --once; the standby unit's too) or a waiting reopen,
// global flags (--config) before the command or not. Alerts and guide
// watches keep their owner's re-arm.
func restarts(p *proc.Process) bool {
	if p.Comm != project.Name || len(p.Args) < 2 {
		return false
	}
	args := p.Args[1:]
	if i := slices.Index(args, watchName); i >= 0 {
		return (i == 0 || !slices.Contains([]string{alertsName, guideName}, args[i-1])) && !slices.Contains(args, "--once")
	}
	i := slices.Index(args, agentsName)
	return i >= 0 && i+1 < len(args) && args[i+1] == reopenName && !slices.Contains(args, "--"+detachFlag)
}

// requestRestarts asks the processes of t that run a binary another file
// was renamed over at exe, and that restart (restarts), to restart on exe
// once, names each and returns them. dry only names them.
// binary says the path of process pid's executable and whether another file
// was renamed over it since.
func requestRestarts(w io.Writer, stateDir, exe string, t *proc.Table, binary func(pid int) (string, bool), now time.Time, dry bool) ([]*proc.Process, error) {
	var due []*proc.Process
	for _, p := range t.ByPID {
		if p.PID == os.Getpid() || !restarts(p) {
			continue
		}
		if path, replaced := binary(p.PID); replaced && path == exe {
			due = append(due, p)
		}
	}
	if len(due) == 0 {
		return nil, nil
	}
	slices.SortFunc(due, func(a, b *proc.Process) int { return a.PID - b.PID })
	for _, p := range due {
		_, _ = fmt.Fprintf(w, "%-7s pid %d (%s): on %s once its current step ends\n", "restart", p.PID, display(p.Args), exe)
	}
	if dry {
		return due, nil
	}
	raw, err := json.Marshal(restartRequest{At: now.UTC(), Binary: exe})
	if err != nil {
		return nil, err
	}
	return due, writeRestart(filepath.Join(stateDir, restartFile), raw)
}

// restartReplaced asks the watches and waiting reopens of t that still run
// the binary an update replaced at exe to restart on it once
// (requestRestarts), and returns them.
func (a *app) restartReplaced(w io.Writer, exe string, t *proc.Table, dry bool) ([]*proc.Process, error) {
	return requestRestarts(w, a.cfg.StateDir, exe, t, processBinary, time.Now(), dry)
}

// processBinary is the path of process pid's executable and whether
// another file was renamed over it since the process started.
func processBinary(pid int) (string, bool) {
	b := platform.ProcessBinary(pid)
	if b == nil {
		return "", false
	}
	return b.Path, b.Replaced()
}

// writeRestart replaces the restart file in one rename.
func writeRestart(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+restartFile+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// restartWatch tells a long-running process when an install asked it to
// restart on the installed binary.
type restartWatch struct {
	file string
	// since is the newest request time this process already answered: its
	// start, or the request it was re-executed for.
	since time.Time
	// binary says the running binary's path and whether another file was
	// renamed over it; nil reads this process's.
	binary func() (string, bool)
}

// newRestartWatch is this process's restart watch over the state
// directory's request.
func newRestartWatch(stateDir string) *restartWatch {
	r := &restartWatch{file: filepath.Join(stateDir, restartFile)}
	if ns, err := strconv.ParseInt(os.Getenv(restartedEnv), 10, 64); err == nil {
		r.since = time.Unix(0, ns)
	} else if s, err := proc.Started(os.Getpid()); err == nil {
		r.since = s
	} else {
		r.since = time.Now()
	}
	return r
}

// due is the request this process restarts for, nil when none: one newer
// than the last it answered, for the binary path this process runs, which
// another file replaced since.
func (r *restartWatch) due() *restartRequest {
	raw, err := os.ReadFile(r.file)
	if err != nil {
		return nil
	}
	var req restartRequest
	if json.Unmarshal(raw, &req) != nil || !req.At.After(r.since) {
		return nil
	}
	bin := r.binary
	if bin == nil {
		bin = func() (string, bool) { return processBinary(os.Getpid()) }
	}
	if path, replaced := bin(); !replaced || path != req.Binary {
		return nil
	}
	return &req
}

// exec replaces this process with the installed binary for req; it returns
// only when that binary does not start, and the process then stays on the
// running one, req answered.
func (r *restartWatch) exec(req *restartRequest) error {
	r.since = req.At
	b := platform.RunningBinary()
	if b == nil {
		return errors.New("the running binary is unknown")
	}
	return b.Exec(restartedEnv + "=" + strconv.FormatInt(req.At.UnixNano(), 10))
}

// brokerProcess and restartBroker read and restart the broker unit
// (beekeeper-sandbox.service); the tests replace them.
var (
	brokerProcess = secret.BrokerProcess
	restartBroker = secret.RestartBroker
)

// brokerStep is what an install does about the broker unit.
type brokerStep int

const (
	// brokerCurrent: no broker, one on another binary, or one started on the
	// installed one.
	brokerCurrent brokerStep = iota
	// brokerRestart: the broker runs the binary replaced at the path and
	// serves no call.
	brokerRestart
	// brokerBusy: the broker runs the replaced binary and serves calls,
	// which a restart would end.
	brokerBusy
)

// brokerDue decides the broker unit's restart: its main process pid, run
// from bin, against the binary written at exe at written. A broker serves a
// call as a child process of its own (a gate, a secret call, an agents
// start); the op daemon its vault session started outlives its parent and
// is none. It answers the calls the broker serves.
func brokerDue(pid int, bin, exe string, written time.Time, t *proc.Table) (brokerStep, int) {
	p := t.ByPID[pid]
	if p == nil || resolvedPath(bin) != resolvedPath(exe) || !p.Start.Before(written) {
		return brokerCurrent, 0
	}
	calls := 0
	for _, c := range t.ByPID {
		if c.PPID == pid {
			calls++
		}
	}
	if calls > 0 {
		return brokerBusy, calls
	}
	return brokerRestart, 0
}

// restartBrokerOn puts the broker unit onto the binary an install wrote at
// exe at written: restarted when it runs the replaced one and serves no
// call, else the restart still due named with its command. A restart drops
// the broker's vault session, said with it. dry only names the step.
func restartBrokerOn(ctx context.Context, w io.Writer, exe string, written time.Time, t *proc.Table, dry bool) {
	pid, bin, err := brokerProcess()
	if err != nil {
		return
	}
	const dropped = "its vault session goes with it, the next secret step waits for the broker's sign-in"
	due := func(why string) {
		_, _ = fmt.Fprintf(w, "%-7s %s (pid %d) stays on the replaced binary %s: systemctl --user restart %s puts it on %s; %s\n",
			"due", secret.BrokerUnit, pid, why, secret.BrokerUnit, exe, dropped)
	}
	step, calls := brokerDue(pid, bin, exe, written, t)
	switch step {
	case brokerBusy:
		due(fmt.Sprintf("while it serves %d call(s), a restart would end them", calls))
	case brokerRestart:
		if !dry {
			if err := restartBroker(ctx); err != nil {
				due(fmt.Sprintf("(%v)", err))
				return
			}
		}
		_, _ = fmt.Fprintf(w, "%-7s %s (pid %d): on %s now; %s\n", "restart", secret.BrokerUnit, pid, exe, dropped)
	}
}

// resolvedPath is path with its symbolic links resolved, else path.
func resolvedPath(path string) string {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	return path
}
