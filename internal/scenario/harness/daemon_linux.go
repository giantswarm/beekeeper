//go:build scenario && linux

package harness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The claude daemon of a machine, as the world keeps it: one claude daemon
// process that runs the machine's background sessions, and what the world
// knows of each session.

type daemon struct {
	cmd *exec.Cmd
	mu  sync.Mutex
	in  io.WriteCloser
	log *os.File
}

func (d *daemon) order(o daemonOrder) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return json.NewEncoder(d.in).Encode(o)
}

func (d *daemon) kill() {
	d.mu.Lock()
	_ = d.in.Close()
	d.mu.Unlock()
	_ = syscall.Kill(-d.cmd.Process.Pid, syscall.SIGKILL)
}

// bgSession is one background session of a machine's claude daemon.
type bgSession struct {
	machine, id, job, name, model, dir string
	env                                []string
	bin                                string
	running                            bool
	// pid is the session's CLI, hostPID its terminal host.
	pid, hostPID int
	exited       chan struct{}
}

// daemonFor is the machine's claude daemon, started with the first
// session's environment as the real one is. The caller holds w.mu.
func (w *World) daemonFor(machine, bin, dir string, env []string, pid int) (*daemon, error) {
	if d := w.daemons[machine]; d != nil {
		return d, nil
	}
	spawnedBy, _ := json.Marshal(map[string]any{"label": "claude --bg", "cwd": dir, "pid": pid})
	cmd := exec.Command(bin, daemonCmd, "run", "--origin", "transient", "--spawned-by", string(spawnedBy)) //nolint:gosec // the scenario's fake claude
	cmd.Env = append(slices.DeleteFunc(slices.Clone(env), func(kv string) bool { return strings.HasPrefix(kv, spawnedEnv+"=") }), spawnedEnv+"=1")
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	d := &daemon{cmd: cmd, in: in}
	if w.opts.Artifacts != "" {
		d.log, _ = os.Create(filepath.Join(w.opts.Artifacts, "claude-daemon-"+machine+".log"))
	}
	if d.log != nil {
		cmd.Stderr = d.log
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	w.daemons[machine] = d
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var e daemonEvent
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue
			}
			w.daemonEvent(machine, e)
		}
		_ = cmd.Wait()
		if d.log != nil {
			_ = d.log.Close()
		}
		// The daemon's end is the end of every session it ran.
		w.mu.Lock()
		if w.daemons[machine] == d {
			delete(w.daemons, machine)
		}
		for _, b := range w.sessions {
			if b.machine == machine && b.running {
				b.running = false
				close(b.exited)
			}
		}
		w.mu.Unlock()
	}()
	return d, nil
}

func (w *World) daemonEvent(machine string, e daemonEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range w.sessions {
		if b.machine != machine || b.job != e.Job || !b.running {
			continue
		}
		switch e.Event {
		case "started":
			b.hostPID = e.PID
		case "exited":
			b.running = false
			close(b.exited)
		}
	}
}

// spawn has the daemon run the session's CLI with argv. The caller holds
// w.mu.
func (w *World) spawn(b *bgSession, pid int, argv []string) error {
	d, err := w.daemonFor(b.machine, b.bin, b.dir, b.env, pid)
	if err != nil {
		return err
	}
	b.running, b.pid, b.hostPID, b.exited = true, 0, 0, make(chan struct{})
	return d.order(daemonOrder{Op: "spawn", Job: b.job, Argv: append([]string{b.bin}, argv...), Dir: b.dir, Env: b.env})
}

// bgStart starts the background session claude --bg asks for.
func (w *World) bgStart(machine string, req request) reply {
	args := req.Args
	name := argValue(args, "-n", "--name")
	if name == "" {
		name = argValue(args, "--remote-control")
	}
	prompt := lastPrompt(args)
	id := newUUID()
	b := &bgSession{machine: machine, id: id, job: id[:8], name: name, model: argValue(args, "--model"), dir: req.Dir, env: sessionEnv(req.Env), bin: req.Exe}
	argv := []string{claudeFlagSession, id}
	if b.model != "" {
		argv = append(argv, "--model", b.model)
	}
	argv = append(argv, "-n", name, "--permission-mode", cmpOr(argValue(args, "--permission-mode"), "default"), "--allow-dangerously-skip-permissions", prompt)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.spawn(b, req.PID, argv); err != nil {
		return reply{Exit: 1, Message: "claude --bg: " + err.Error()}
	}
	w.sessions = append(w.sessions, b)
	return reply{Short: b.job, Message: name}
}

// sessionEnv is a launcher's environment as its session inherits it.
func sessionEnv(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool { return strings.HasPrefix(kv, spawnedEnv+"=") })
}

// findSession is the machine's session that ref names: its id, a prefix
// of it or its job id. The newest wins. The caller holds w.mu.
func (w *World) findSession(machine, ref string) *bgSession {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	for _, b := range slices.Backward(w.sessions) {
		if b.machine == machine && (b.id == ref || b.job == ref || strings.HasPrefix(b.id, ref)) {
			return b
		}
	}
	return nil
}

// bgResume continues a stopped session with a message as its next turn
// (claude --bg --resume <id> <message>). A session that still runs gets a
// copy beside it under a new id, as the real CLI starts one.
func (w *World) bgResume(machine string, req request) reply {
	args := req.Args
	ref := argValue(args, claudeFlagResume, "-r")
	msg := ""
	if i := slices.Index(args, ref); i >= 0 && i+1 < len(args) {
		msg = strings.Join(args[i+1:], " ")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	b := w.findSession(machine, strings.TrimSuffix(filepath.Base(ref), ".jsonl"))
	if b == nil {
		return reply{Exit: 1, Message: "No conversation found with session ID: " + ref}
	}
	if b.running {
		id := newUUID()
		cp := &bgSession{machine: machine, id: id, job: id[:8], name: b.name, model: b.model, dir: b.dir, env: b.env, bin: b.bin}
		argv := []string{claudeFlagSession, id}
		if cp.model != "" {
			argv = append(argv, "--model", cp.model)
		}
		argv = append(argv, "-n", cp.name, "--", msg)
		if err := w.spawn(cp, req.PID, argv); err != nil {
			return reply{Exit: 1, Message: "claude --bg --resume: " + err.Error()}
		}
		w.sessions = append(w.sessions, cp)
		return reply{Short: cp.job, Message: cp.name}
	}
	argv := []string{claudeFlagResume, transcriptPath(configDirOf(b.env), b.dir, b.id), "-n", b.name}
	if b.model != "" {
		argv = append(argv, "--model", b.model)
	}
	argv = append(argv, "--", msg)
	if err := w.spawn(b, req.PID, argv); err != nil {
		return reply{Exit: 1, Message: "claude --bg --resume: " + err.Error()}
	}
	return reply{Short: b.job, Message: b.name}
}

func configDirOf(env []string) string {
	home := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "CLAUDE_CONFIG_DIR="); ok && v != "" {
			return v
		}
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			home = v
		}
	}
	return filepath.Join(home, ".claude")
}

// bgList are the rows of claude agents --json: the running sessions, with
// all the stopped ones too.
func (w *World) bgList(machine string, all bool) []bgRow {
	w.mu.Lock()
	defer w.mu.Unlock()
	rows := []bgRow{}
	for _, b := range w.sessions {
		if b.machine != machine || !b.running && !all {
			continue
		}
		row := bgRow{ID: b.job, SessionID: b.id, Name: b.name, Kind: "background", Status: "stopped"}
		if b.running {
			row.PID, row.Status = cmpOrInt(b.pid, b.hostPID), "running"
		}
		rows = append(rows, row)
	}
	return rows
}

func cmpOrInt(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// bgStop ends a session's CLI (claude stop), keeping its conversation.
func (w *World) bgStop(machine, ref string) reply {
	w.mu.Lock()
	b := w.findSession(machine, ref)
	if b == nil {
		w.mu.Unlock()
		return reply{Exit: 1, Message: "No background session matches " + ref}
	}
	if !b.running {
		w.mu.Unlock()
		return reply{Message: fmt.Sprintf("%s is not running", b.job)}
	}
	d, exited := w.daemons[machine], b.exited
	w.mu.Unlock()
	if d != nil {
		_ = d.order(daemonOrder{Op: "kill", Job: b.job})
	}
	select {
	case <-exited:
	case <-time.After(stopWait):
		w.mu.Lock()
		if b.hostPID > 0 {
			_ = syscall.Kill(b.hostPID, syscall.SIGKILL)
		}
		if b.pid > 0 {
			_ = syscall.Kill(b.pid, syscall.SIGKILL)
		}
		w.mu.Unlock()
		select {
		case <-exited:
		case <-time.After(stopWait):
		}
	}
	return reply{Message: fmt.Sprintf("stopped · %s · %s", b.job, b.name)}
}
