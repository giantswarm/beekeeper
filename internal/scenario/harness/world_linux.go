//go:build scenario && linux

package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Options are what a scenario's driver gives the world of its fakes.
type Options struct {
	Given Given
	Hosts []Host
	// Env is the desk's own environment: what a transient unit of the
	// desk's service manager starts with.
	Env []string
	// Artifacts receives the logs of what the world runs: the units, the
	// claude daemons and every worker's turns.
	Artifacts string
	// Answer is the scenario's answer to a call: a given call's (given),
	// else, for a call the fake models, the note that the fake answers it
	// itself. A call neither answers is logged as unanswered and gets the
	// refusal Answer returns.
	Answer func(program string, args []string, stdin, dir string, modeled bool) (stdout, stderr string, exit int, given bool)
}

// World holds the state of every fake of the package for one scenario and
// answers them over a Unix socket: the service manager of each machine,
// its claude daemon, the workers' turns, the providers' usage and the
// hosts' faults.
type World struct {
	opts Options

	mu       sync.Mutex
	units    map[string]*unit
	daemons  map[string]*daemon
	sessions []*bgSession
	workers  map[string]*workerState
	order    []string
	usage    map[string]Usage
	full     map[string]bool
	hosts    map[string]*hostState
	failures []string
	turnLog  *os.File

	ln net.Listener
	wg sync.WaitGroup
}

// hostState is an execution host as the scenario has it now.
type hostState struct {
	host  Host
	fault string
	slow  time.Duration
}

// workerState is what the sessions of one worker name did.
type workerState struct {
	machine, harness, name string
	turns                  int
	reports                []report
	pids                   []int
}

// NewWorld is the world of a scenario's fakes.
func NewWorld(opts Options) (*World, error) {
	w := &World{
		opts: opts, units: map[string]*unit{}, daemons: map[string]*daemon{},
		workers: map[string]*workerState{}, usage: map[string]Usage{}, full: map[string]bool{}, hosts: map[string]*hostState{},
	}
	for name, u := range opts.Given.Usage {
		w.usage[name] = u
	}
	for _, h := range opts.Hosts {
		w.hosts[h.Name] = &hostState{host: h, fault: h.Fault, slow: h.Slow.Duration}
	}
	if opts.Artifacts != "" {
		if err := os.MkdirAll(filepath.Join(opts.Artifacts, "units"), 0o700); err != nil {
			return nil, err
		}
		f, err := os.Create(filepath.Join(opts.Artifacts, "workers.jsonl"))
		if err != nil {
			return nil, err
		}
		w.turnLog = f
	}
	return w, nil
}

// Serve listens on the Unix socket path and answers until Close.
func (w *World) Serve(path string) error {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	w.ln = ln
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			w.wg.Add(1)
			go func() {
				defer w.wg.Done()
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(time.Minute))
				var req request
				if err := json.NewDecoder(conn).Decode(&req); err != nil {
					_ = json.NewEncoder(conn).Encode(reply{Error: "unreadable request: " + err.Error()})
					return
				}
				_ = json.NewEncoder(conn).Encode(w.handle(req))
			}()
		}
	}()
	return nil
}

// Close stops answering and ends what the world runs: every unit and
// every claude daemon with its sessions.
func (w *World) Close() error {
	var err error
	if w.ln != nil {
		if e := w.ln.Close(); e != nil && !errors.Is(e, net.ErrClosed) {
			err = e
		}
		w.wg.Wait()
	}
	w.mu.Lock()
	units := make([]*unit, 0, len(w.units))
	for _, u := range w.units {
		units = append(units, u)
	}
	daemons := make([]*daemon, 0, len(w.daemons))
	for _, d := range w.daemons {
		daemons = append(daemons, d)
	}
	w.mu.Unlock()
	for _, u := range units {
		u.kill()
	}
	for _, d := range daemons {
		d.kill()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.turnLog != nil {
		_ = w.turnLog.Close()
		w.turnLog = nil
	}
	return err
}

// Failures are the scenario's failures the fakes found: a worker's step
// that did not end as its script says, a turn that could not run.
func (w *World) Failures() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.failures)
}

func (w *World) fail(format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failures = append(w.failures, fmt.Sprintf(format, args...))
}

// machineEnv is the environment of a machine's service manager and of a
// command an ssh call runs there.
func (w *World) machineEnv(machine string) []string {
	if machine == LocalHost || machine == "" {
		return slices.Clone(w.opts.Env)
	}
	return HostEnv(w.opts.Env, machine)
}

func (w *World) handle(req request) reply {
	machine := MachineOf(req.Dir, req.Home, req.Exe)
	switch req.Op {
	case opCall:
		rep := reply{Machine: machine}
		if w.opts.Answer != nil {
			stdout, stderr, exit, given := w.opts.Answer(req.Program, req.Args, req.Stdin, req.Dir, req.Modeled)
			if given || !req.Modeled {
				rep.Given = &canned{Stdout: stdout, Stderr: stderr, Exit: exit}
			}
		}
		return rep
	case opTurn:
		return w.turn(machine, req)
	case opTurnDone:
		return w.turnDone(machine, req)
	case opSessionPID:
		w.sessionPID(machine, req)
		return reply{}
	case opUnitStart:
		return w.unitStart(machine, req)
	case opUnitVerb:
		return w.unitVerb(machine, req)
	case opUnitState:
		return reply{State: w.unitState(machine, firstOf(req.Names))}
	case opUnitList:
		return reply{Lines: w.unitList(machine, req.States, req.Names)}
	case opBgStart:
		return w.bgStart(machine, req)
	case opBgResume:
		return w.bgResume(machine, req)
	case opBgList:
		return reply{Rows: w.bgList(machine, req.All)}
	case opBgStop:
		return w.bgStop(machine, firstOf(req.Names))
	case opUsage:
		w.mu.Lock()
		defer w.mu.Unlock()
		u := w.usage[req.Harness]
		return reply{Usage: &u, Full: w.full[req.Harness]}
	case opModels:
		models := w.opts.Given.Models[req.Harness]
		if len(models) == 0 {
			models = DefaultModels[req.Harness]
		}
		return reply{Models: models}
	case opSSH:
		return w.ssh(req.Dest)
	}
	return reply{Error: "unknown operation " + req.Op}
}

func firstOf(xs []string) string {
	if len(xs) == 0 {
		return ""
	}
	return xs[0]
}

// script is the worker script of a session, nil for none.
func (w *World) script(machine, harness, name string) *Worker {
	for i := range w.opts.Given.Workers {
		wk := &w.opts.Given.Workers[i]
		if wk.Harness != "" && wk.Harness != harness {
			continue
		}
		if wk.Host != "" && wk.Host != machine {
			continue
		}
		if wk.Name != "" {
			re, err := regexp.Compile(`^(?:` + wk.Name + `)$`)
			if err != nil || !re.MatchString(name) {
				continue
			}
		}
		return wk
	}
	return nil
}

func workerKey(machine, harness, name string) string { return machine + "|" + harness + "|" + name }

// turn hands a session its next turn.
func (w *World) turn(machine string, req request) reply {
	s := req.Session
	if s == nil {
		return reply{Error: "turn without a session"}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	key := workerKey(machine, s.Harness, s.Name)
	ws := w.workers[key]
	if ws == nil {
		ws = &workerState{machine: machine, harness: s.Harness, name: s.Name}
		w.workers[key] = ws
		w.order = append(w.order, key)
	}
	plan := &turnPlan{Index: ws.turns, Outcome: Finish, Reply: "done", Tokens: Tokens{Input: 1200, Output: 300}}
	ws.turns++
	if req.PID > 0 && !slices.Contains(ws.pids, req.PID) {
		ws.pids = append(ws.pids, req.PID)
	}
	if wk := w.script(machine, s.Harness, s.Name); wk != nil {
		var t *Turn
		switch {
		case plan.Index < len(wk.Turns):
			t = &wk.Turns[plan.Index]
		case wk.Repeat:
			t = &wk.Turns[len(wk.Turns)-1]
		}
		if t != nil {
			plan.Steps = t.Steps
			if t.Outcome != "" {
				plan.Outcome = t.Outcome
			}
			if t.Reply != "" {
				plan.Reply = t.Reply
			}
			if t.Tokens != nil {
				plan.Tokens = *t.Tokens
			}
			plan.Exit = t.Exit
			if plan.Outcome == Crash && plan.Exit == 0 {
				plan.Exit = 1
			}
		}
	}
	plan.Vars = map[string]string{
		"name": s.Name, "session": s.ID, "harness": s.Harness, "dir": req.Dir,
		"turn": fmt.Sprint(plan.Index), "host": machine,
	}
	for _, b := range w.sessions {
		if b.machine == machine && s.Harness == Claude && b.id == s.ID && req.PID > 0 {
			b.pid = req.PID
		}
	}
	return reply{Turn: plan, Machine: machine}
}

// turnDone records a session's account of a turn.
func (w *World) turnDone(machine string, req request) reply {
	s, r := req.Session, req.Report
	if s == nil || r == nil {
		return reply{Error: "turn-done without a session or a report"}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	key := workerKey(machine, s.Harness, s.Name)
	ws := w.workers[key]
	if ws == nil {
		return reply{Error: "turn-done of a worker that began no turn"}
	}
	ws.reports = append(ws.reports, *r)
	if r.Failure != "" {
		w.failures = append(w.failures, fmt.Sprintf("worker %q (%s on %s) turn %d: %s", s.Name, s.Harness, machine, r.Index, r.Failure))
	}
	if wk := w.script(machine, s.Harness, s.Name); wk != nil && len(wk.Turns) > 0 {
		t := wk.Turns[min(r.Index, len(wk.Turns)-1)]
		if r.Index < len(wk.Turns) || wk.Repeat {
			if t.Usage != nil {
				w.usage[s.Harness] = *t.Usage
			}
			if t.Outcome == Budget {
				w.full[s.Harness] = true
				u := w.usage[s.Harness]
				u.Session = 100
				w.usage[s.Harness] = u
			}
		}
	}
	if w.turnLog != nil {
		line, err := json.Marshal(struct {
			At      time.Time `json:"at"`
			Machine string    `json:"machine"`
			Harness string    `json:"harness"`
			Name    string    `json:"name"`
			Session string    `json:"session"`
			report
		}{time.Now().UTC(), machine, s.Harness, s.Name, s.ID, *r})
		if err == nil {
			_, _ = w.turnLog.Write(append(line, '\n'))
		}
	}
	return reply{}
}

func (w *World) sessionPID(machine string, req request) {
	if req.Session == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range w.sessions {
		if b.machine == machine && b.id == req.Session.ID {
			b.pid = req.PID
		}
	}
}

// CheckWorker checks a worker expectation against what the sessions did.
func (w *World) CheckWorker(e WorkerExpect) error {
	re, err := regexp.Compile(`^(?:` + e.Name + `)$`)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	turns, outcome, open, running := 0, "", false, false
	var names []string
	for _, key := range w.order {
		ws := w.workers[key]
		names = append(names, fmt.Sprintf("%s (%s on %s)", ws.name, ws.harness, ws.machine))
		if e.Harness != "" && ws.harness != e.Harness || e.Host != "" && ws.machine != e.Host || !re.MatchString(ws.name) {
			continue
		}
		turns += ws.turns
		// A turn counts once it is handed out and ends with its report:
		// while one has not reported, the worker's last turn has no
		// outcome, and an earlier turn's is not it.
		outcome, open = "", len(ws.reports) < ws.turns
		if n := len(ws.reports); n > 0 && !open {
			outcome = ws.reports[n-1].Outcome
		}
		for _, pid := range ws.pids {
			if alive(pid) {
				running = true
			}
		}
	}
	what := "worker " + cmpOr(e.Name, "(any)")
	if e.Harness != "" {
		what += " on " + e.Harness
	}
	if e.Host != "" {
		what += " at " + e.Host
	}
	if e.Turns != nil && turns != *e.Turns {
		return fmt.Errorf("%s: %d turn(s), want %d (workers: %s)", what, turns, *e.Turns, cmpOr(strings.Join(names, ", "), "none"))
	}
	if e.Outcome != "" && outcome != e.Outcome {
		if open {
			return fmt.Errorf("%s: its last turn has not ended yet, want it ended %q", what, e.Outcome)
		}
		return fmt.Errorf("%s: its last reported turn ended %q, want %q", what, cmpOr(outcome, "not at all"), e.Outcome)
	}
	if e.Running != nil && running != *e.Running {
		return fmt.Errorf("%s: a CLI of it running %t, want %t", what, running, *e.Running)
	}
	return nil
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ApplyHostStep changes a host's faults.
func (w *World) ApplyHostStep(h HostStep) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	hs := w.hosts[h.Name]
	if hs == nil {
		return fmt.Errorf("host %q is none of given.hosts", h.Name)
	}
	switch {
	case h.Clear:
		hs.fault, hs.slow = "", 0
	case h.Fault != "":
		hs.fault = h.Fault
	}
	if h.Slow.Duration > 0 {
		hs.slow = h.Slow.Duration
	}
	return nil
}

// ssh is where an ssh destination leads: a host's root, or the desk for
// the name the hosts call their coordinator back under.
func (w *World) ssh(dest string) reply {
	if i := strings.LastIndexByte(dest, '@'); i >= 0 {
		dest = dest[i+1:]
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if dest == CoordinatorAlias {
		return reply{SSH: &sshRoute{Root: "/work", Home: homeOf(w.opts.Env), Env: slices.Clone(w.opts.Env)}}
	}
	hs := w.hosts[dest]
	if hs == nil {
		return reply{Exit: 255, Message: fmt.Sprintf("ssh: Could not resolve hostname %s: Name or service not known", dest)}
	}
	fault := hs.fault
	if fault == OlderGo {
		fault = ""
	}
	root := HostRoot(dest)
	return reply{SSH: &sshRoute{Root: root, Home: filepath.Join(root, "home"), Env: HostEnv(w.opts.Env, dest), Fault: fault, Slow: hs.slow.Milliseconds()}}
}

func homeOf(env []string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			return v
		}
	}
	return "/"
}
