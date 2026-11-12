//go:build scenario && linux

package harness

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// The service manager of a machine: the world runs the transient units
// systemd-run asks for as its own children, as the user manager does, and
// answers systemctl about them.

// A unit's states, as systemctl names them.
const (
	unitActive       = "active"
	unitDeactivating = "deactivating"
	unitInactive     = "inactive"
	unitFailed       = "failed"
)

// stopWait bounds a unit's stop: the real TimeoutStopSec, but no scenario
// waits a minute and a half for a unit that ignores SIGTERM.
const stopWait = 5 * time.Second

type unit struct {
	spec    unitSpec
	machine string
	name    string
	cmd     *exec.Cmd
	done    chan struct{}

	mu       sync.Mutex
	state    string
	timedOut bool
}

func (u *unit) getState() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.state
}

// serviceName is a unit's full name.
func serviceName(name string) string {
	if strings.Contains(path.Base(name), ".") {
		return name
	}
	return name + ".service"
}

func unitKey(machine, name string) string { return machine + "/" + serviceName(name) }

// lookPath resolves file against the PATH of env, as the service manager
// resolves a unit's command.
func lookPath(file string, env []string) (string, error) {
	if strings.Contains(file, "/") {
		return file, nil
	}
	for _, kv := range env {
		v, ok := strings.CutPrefix(kv, "PATH=")
		if !ok {
			continue
		}
		for _, dir := range filepath.SplitList(v) {
			p := filepath.Join(dir, file)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("%s: executable file not found in the unit's PATH", file)
}

// mergeEnv lays extra (KEY=value) over base.
func mergeEnv(base, extra []string) []string {
	out := slices.Clone(base)
	for _, kv := range extra {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		out = slices.DeleteFunc(out, func(e string) bool { return strings.HasPrefix(e, k+"=") })
		out = append(out, kv)
	}
	return out
}

// unitStart starts a transient service.
func (w *World) unitStart(machine string, req request) reply {
	spec := req.Unit
	if spec == nil || spec.Name == "" || len(spec.Argv) == 0 {
		return reply{Exit: 1, Message: "Failed to start transient service unit: no unit or command"}
	}
	name := serviceName(spec.Name)
	key := unitKey(machine, name)
	w.mu.Lock()
	if old := w.units[key]; old != nil {
		if st := old.getState(); st == unitActive || st == unitDeactivating || st == unitFailed {
			w.mu.Unlock()
			return reply{Exit: 1, Message: fmt.Sprintf("Failed to start transient service unit: Unit %s was already loaded or has a fragment file.", name)}
		}
	}
	// The unit's processes carry its name as they would sit in its cgroup
	// (proc.ScenarioUnitEnv): a sandbox can create none.
	env := mergeEnv(w.machineEnv(machine), append(slices.Clone(spec.Env), proc.ScenarioUnitEnv+"="+name))
	bin, err := lookPath(spec.Argv[0], env)
	if err != nil {
		w.mu.Unlock()
		return reply{Exit: 1, Message: "Failed to start transient service unit: " + err.Error()}
	}
	cmd := exec.Command(bin, spec.Argv[1:]...) //nolint:gosec // the unit beekeeper asked the scenario's service manager for
	cmd.Args[0] = spec.Argv[0]
	cmd.Env = env
	cmd.Dir = spec.Dir
	if cmd.Dir == "" {
		cmd.Dir = homeOf(env)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var log *os.File
	if w.opts.Artifacts != "" {
		log, _ = os.OpenFile(filepath.Join(w.opts.Artifacts, "units", machine+"-"+name+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // the scenario's artifacts
	}
	if log != nil {
		cmd.Stdout, cmd.Stderr = log, log
	}
	if err := cmd.Start(); err != nil {
		w.mu.Unlock()
		if log != nil {
			_ = log.Close()
		}
		return reply{Exit: 1, Message: "Failed to start transient service unit: " + err.Error()}
	}
	u := &unit{spec: *spec, machine: machine, name: name, cmd: cmd, done: make(chan struct{}), state: unitActive}
	w.units[key] = u
	w.mu.Unlock()
	if spec.MaxRuntime > 0 {
		time.AfterFunc(time.Duration(spec.MaxRuntime)*time.Second, func() {
			u.mu.Lock()
			if u.state == unitActive {
				u.timedOut = true
			}
			u.mu.Unlock()
			u.stop()
		})
	}
	go func() {
		err := cmd.Wait()
		if log != nil {
			_ = log.Close()
		}
		// KillMode=process leaves what the main process started running, as
		// a terminal would; any other mode ends the unit's processes with it.
		if spec.KillMode != "process" {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		clean := cleanEnd(cmd.ProcessState, err, spec.Success)
		if post := strings.TrimPrefix(strings.TrimSpace(spec.StopPost), "-"); post != "" {
			if f := strings.Fields(post); len(f) > 0 {
				if p, perr := lookPath(f[0], env); perr == nil {
					c := exec.Command(p, f[1:]...) //nolint:gosec // the unit's own ExecStopPost
					c.Env, c.Dir = env, cmd.Dir
					c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
					if c.Start() == nil {
						waited := make(chan struct{})
						go func() { _ = c.Wait(); close(waited) }()
						select {
						case <-waited:
						case <-time.After(stopWait):
							_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
							<-waited
						}
					}
				}
			}
		}
		u.mu.Lock()
		if clean && !u.timedOut {
			u.state = unitInactive
		} else {
			u.state = unitFailed
		}
		u.mu.Unlock()
		// --collect unloads the unit once it ended, failed or not.
		if spec.Collect {
			w.mu.Lock()
			if w.units[key] == u {
				delete(w.units, key)
			}
			w.mu.Unlock()
		}
		close(u.done)
	}()
	return reply{}
}

// cleanEnd says whether a unit's main process ended as a success: exit 0,
// one of the signals a service manager counts as a clean stop, or what
// SuccessExitStatus adds.
func cleanEnd(ps *os.ProcessState, waitErr error, success []string) bool {
	if ps == nil {
		return waitErr == nil
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok {
		return ps.Success()
	}
	if ws.Signaled() {
		switch ws.Signal() {
		case syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT, syscall.SIGPIPE:
			return true
		}
		return slices.Contains(success, signalName(ws.Signal()))
	}
	return ws.ExitStatus() == 0 || slices.Contains(success, strconv.Itoa(ws.ExitStatus()))
}

func signalName(s syscall.Signal) string {
	switch s {
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGABRT:
		return "SIGABRT"
	}
	return "SIG" + strconv.Itoa(int(s))
}

// stop ends the unit as systemctl stop does: SIGTERM to its main process,
// SIGKILL to what is left once that ended or the stop timed out.
func (u *unit) stop() {
	u.mu.Lock()
	if u.state != unitActive && u.state != unitDeactivating {
		u.mu.Unlock()
		return
	}
	u.state = unitDeactivating
	u.mu.Unlock()
	pid := u.cmd.Process.Pid
	_ = syscall.Kill(pid, syscall.SIGTERM)
	wait := stopWait
	if t := time.Duration(u.spec.StopTimeout) * time.Second; t > 0 && t < wait {
		wait = t
	}
	select {
	case <-u.done:
	case <-time.After(wait):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-u.done
	}
}

// kill ends the unit's processes at the scenario's end.
func (u *unit) kill() {
	select {
	case <-u.done:
		return
	default:
	}
	_ = syscall.Kill(-u.cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-u.done:
	case <-time.After(stopWait):
	}
}

func (w *World) unitState(machine, name string) string {
	w.mu.Lock()
	u := w.units[unitKey(machine, name)]
	w.mu.Unlock()
	if u == nil {
		return unitInactive
	}
	return u.getState()
}

// unitList lists the units of a machine in one of states matching one of
// the patterns (every unit without patterns), as systemctl list-units
// --plain --no-legend prints them.
func (w *World) unitList(machine string, states, patterns []string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for key, u := range w.units {
		if !strings.HasPrefix(key, machine+"/") {
			continue
		}
		st := u.getState()
		if len(states) > 0 && !slices.Contains(states, st) {
			continue
		}
		if len(patterns) > 0 && !slices.ContainsFunc(patterns, func(p string) bool {
			ok, _ := path.Match(p, u.name)
			if !ok {
				ok, _ = path.Match(serviceName(p), u.name)
			}
			return ok
		}) {
			continue
		}
		sub := "running"
		switch st {
		case unitFailed:
			sub = "failed"
		case unitInactive:
			sub = "dead"
		case unitDeactivating:
			sub = "stop-sigterm"
		}
		out = append(out, fmt.Sprintf("%s loaded %s %s %s", u.name, st, sub, strings.Join(u.spec.Argv, " ")))
	}
	slices.Sort(out)
	return out
}

// unitVerb runs one systemctl verb on the named units.
func (w *World) unitVerb(machine string, req request) reply {
	var problems []string
	for _, name := range req.Names {
		key := unitKey(machine, name)
		w.mu.Lock()
		u := w.units[key]
		w.mu.Unlock()
		switch req.Verb {
		case "stop":
			if u != nil {
				u.stop()
			}
		case "kill":
			if u != nil {
				_ = syscall.Kill(-u.cmd.Process.Pid, syscall.SIGKILL)
			}
		case "freeze", "thaw":
			if u == nil || u.getState() != unitActive {
				problems = append(problems, fmt.Sprintf("Failed to %s unit %s: Unit %s not loaded.", req.Verb, serviceName(name), serviceName(name)))
				continue
			}
			sig := syscall.SIGSTOP
			if req.Verb == "thaw" {
				sig = syscall.SIGCONT
			}
			_ = syscall.Kill(-u.cmd.Process.Pid, sig)
		case "start", "restart":
			// A transient unit that ended is gone: nothing starts it again.
			if u == nil || u.getState() != unitActive {
				problems = append(problems, fmt.Sprintf("Failed to %s %s: Unit %s not found.", req.Verb, serviceName(name), serviceName(name)))
			}
		case "try-restart":
		case "reset-failed":
			w.mu.Lock()
			if u != nil && u.getState() == unitFailed {
				delete(w.units, key)
			}
			w.mu.Unlock()
		default:
			return reply{Exit: 1, Message: "Unknown command verb '" + req.Verb + "'."}
		}
	}
	if len(problems) > 0 {
		return reply{Exit: 5, Message: strings.Join(problems, "\n")}
	}
	return reply{}
}

// The clients.

// systemdRun is the fake systemd-run: --scope runs the command in place,
// as systemd-run's child would run; anything else asks the world's service
// manager for a transient unit.
func systemdRun(c *client) int {
	spec := unitSpec{KillMode: "control-group"}
	scope := false
	args := c.args
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		if !strings.HasPrefix(a, "-") {
			break
		}
		val := func() string {
			if _, v, ok := strings.Cut(a, "="); ok {
				return v
			}
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "--user", a == "--quiet", a == "-q", a == "--no-block", a == "--expand-environment=no", a == "--same-dir", a == "--wait", a == "--pty", a == "--pipe":
		case a == "--scope":
			scope = true
		case a == "--collect", a == "-G":
			spec.Collect = true
		case strings.HasPrefix(a, "--unit"), a == "-u":
			spec.Name = val()
		case strings.HasPrefix(a, "--working-directory"):
			spec.Dir = val()
		case strings.HasPrefix(a, "--setenv"), a == "-E":
			spec.Env = append(spec.Env, val())
		case strings.HasPrefix(a, "--slice"), strings.HasPrefix(a, "--nice"), strings.HasPrefix(a, "--description"):
			_ = val()
		case strings.HasPrefix(a, "--property"), a == "-p":
			k, v, _ := strings.Cut(val(), "=")
			switch k {
			case "KillMode":
				spec.KillMode = v
			case "SuccessExitStatus":
				spec.Success = strings.Fields(v)
			case "ExecStopPost":
				spec.StopPost = v
			case "TimeoutStopSec":
				spec.StopTimeout, _ = strconv.Atoi(v)
			case "RuntimeMaxSec":
				spec.MaxRuntime, _ = strconv.Atoi(v)
			case "CollectMode":
				spec.Collect = spec.Collect || v == "inactive-or-failed"
			}
		default:
			fmt.Fprintf(os.Stderr, "systemd-run: the scenario's service manager does not know %s\n", a)
			return 1
		}
	}
	spec.Argv = args[i:]
	if len(spec.Argv) == 0 {
		fmt.Fprintln(os.Stderr, "systemd-run: Command line to execute required.")
		return 1
	}
	if scope {
		bin, err := exec.LookPath(spec.Argv[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to find executable %s: No such file or directory\n", spec.Argv[0])
			return 1
		}
		if err := syscall.Exec(bin, spec.Argv, os.Environ()); err != nil { //nolint:gosec // the command the caller wrapped in a scope
			fmt.Fprintf(os.Stderr, "Failed to execute %s: %v\n", spec.Argv[0], err)
			return 1
		}
	}
	if spec.Name == "" {
		spec.Name = "run-u" + strconv.Itoa(os.Getpid())
	}
	rep, err := c.ask(request{Op: opUnitStart, Unit: &spec})
	return c.end(rep, err)
}

// systemctl is the fake systemctl for what beekeeper asks of the user's
// service manager.
func systemctl(c *client) int {
	var verb string
	var names, props, states []string
	value := false
	args := c.args
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--user", a == "--all", a == "--plain", a == "--no-legend", a == "--no-pager", a == "--runtime", a == "--quiet", a == "--now", a == "--no-block":
		case a == "--value":
			value = true
		case a == "-p", a == "--property":
			if i+1 < len(args) {
				i++
				props = append(props, args[i])
			}
		case strings.HasPrefix(a, "--property="):
			props = append(props, strings.TrimPrefix(a, "--property="))
		case strings.HasPrefix(a, "--state="):
			states = strings.Split(strings.TrimPrefix(a, "--state="), ",")
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "systemctl: the scenario's service manager does not know %s\n", a)
			return 1
		case verb == "":
			verb = a
		default:
			names = append(names, a)
		}
	}
	switch verb {
	case "is-system-running":
		fmt.Println("running")
		return 0
	case "show":
		for _, name := range names {
			for _, p := range props {
				v := ""
				switch p {
				case "ActiveState":
					rep, err := c.ask(request{Op: opUnitState, Names: []string{name}})
					if err != nil {
						return c.end(rep, err)
					}
					v = rep.State
				case "OOMPolicy":
					v = "stop"
				}
				if value {
					fmt.Println(v)
				} else {
					fmt.Println(p + "=" + v)
				}
			}
		}
		return 0
	case "is-active":
		rep, err := c.ask(request{Op: opUnitState, Names: names})
		if err != nil {
			return c.end(rep, err)
		}
		fmt.Println(rep.State)
		if rep.State != unitActive {
			return 3
		}
		return 0
	case "list-units":
		rep, err := c.ask(request{Op: opUnitList, Names: names, States: states})
		if err != nil {
			return c.end(rep, err)
		}
		for _, l := range rep.Lines {
			fmt.Println(l)
		}
		return 0
	case "set-property", "daemon-reload":
		return 0
	case "stop", "kill", "start", "restart", "try-restart", "freeze", "thaw", "reset-failed":
		rep, err := c.ask(request{Op: opUnitVerb, Verb: verb, Names: names})
		return c.end(rep, err)
	}
	fmt.Fprintf(os.Stderr, "Unknown command verb '%s'.\n", verb)
	return 1
}

// systemctlModeled says whether the fake systemctl answers args itself.
func systemctlModeled(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		switch a {
		case "is-system-running", "show", "is-active", "list-units", "set-property", "daemon-reload",
			"stop", "kill", "start", "restart", "try-restart", "freeze", "thaw", "reset-failed":
			return true
		}
		return false
	}
	return false
}

var errNoWorld = errors.New("the scenario's world is unreachable")
