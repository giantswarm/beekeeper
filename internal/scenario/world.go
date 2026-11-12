//go:build scenario

package scenario

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/scenario/fakegithub"
)

// WorldEnv names the world's Unix socket to the fakes.
const WorldEnv = "BEEKEEPER_SCENARIO_WORLD"

// CallRequest is one external call a fake hands to the world.
type CallRequest struct {
	Program string   `json:"program"`
	Args    []string `json:"args"`
	Stdin   string   `json:"stdin,omitempty"`
	Dir     string   `json:"dir,omitempty"`
	// Env is the fake's environment as far as a handler reads it (FakeEnv).
	Env map[string]string `json:"env,omitempty"`
	// Modeled says the fake answers the call itself unless a given call
	// of the scenario does: the world logs it as answered by its model.
	Modeled bool `json:"modeled,omitempty"`
}

// CallResponse is what the fake prints and exits with.
type CallResponse struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Exit   int    `json:"exit"`
	// Given says a given call answered; false leaves a modeled call to its
	// fake.
	Given bool `json:"given,omitempty"`
}

// CallRecord is one call in the world's log, in order.
type CallRecord struct {
	Seq     int       `json:"seq"`
	At      time.Time `json:"at"`
	Program string    `json:"program"`
	Args    []string  `json:"args"`
	Dir     string    `json:"dir,omitempty"`
	// Answer is the index of the given call that answered it, -1 for none.
	Answer   int  `json:"answer"`
	Answered bool `json:"answered"`
	// Modeled says the call's fake answered it from its model of the
	// program, no given call; its exit code is the fake's own.
	Modeled bool `json:"modeled,omitempty"`
	Exit    int  `json:"exit"`
	// Handler says the program's handler answered it, not a given call;
	// Reason why nothing answered it, when a handler said.
	Handler bool   `json:"handler,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Line is the call as a command line, shortened for reports.
func (c CallRecord) Line() string {
	parts := make([]string, 0, len(c.Args)+1)
	parts = append(parts, c.Program)
	for _, a := range c.Args {
		if len(a) > 120 {
			a = a[:117] + "..."
		}
		if strings.ContainsAny(a, " \n\t\"'") {
			a = fmt.Sprintf("%q", a)
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

type answer struct {
	call Call
	args []*regexp.Regexp
	used int
}

// Handler answers a program's calls from a model (the fake GitHub for gh):
// its answer, or false and why it has none.
type Handler func(req CallRequest) (resp CallResponse, answered bool, reason string)

// FakeEnv are the variables a fake hands the world with its call.
var FakeEnv = []string{"GH_REPO", "GH_HOST", fakegithub.CallerEnv}

// World holds every fake's state for one scenario and answers their calls
// over a Unix socket. It logs every call in order; a call neither a given
// call nor the program's handler answers fails the scenario.
type World struct {
	mu       sync.Mutex
	answers  []*answer
	handlers map[string]Handler
	calls    []CallRecord
	log      io.Writer
	ln       net.Listener
	wg       sync.WaitGroup
}

// Handle makes h answer the calls of program that no given call answers.
func (w *World) Handle(program string, h Handler) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.handlers == nil {
		w.handlers = map[string]Handler{}
	}
	w.handlers[program] = h
}

// NewWorld is a world answering with calls; log, when set, receives each
// call as a JSON line.
func NewWorld(calls []Call, log io.Writer) (*World, error) {
	w := &World{log: log}
	for i, c := range calls {
		res, err := compileArgs(c.Args)
		if err != nil {
			return nil, fmt.Errorf("given.calls[%d]: %w", i, err)
		}
		w.answers = append(w.answers, &answer{call: c, args: res})
	}
	return w, nil
}

// matchArgs says whether every pattern matches some argument.
func matchArgs(pats []*regexp.Regexp, args []string) bool {
	for _, re := range pats {
		if !slices.ContainsFunc(args, re.MatchString) {
			return false
		}
	}
	return true
}

// Answer answers one call and logs it: with the first given call that
// matches, else a modeled call with its fake's model, else with the
// program's handler.
func (w *World) Answer(req CallRequest) CallResponse {
	rec := CallRecord{At: time.Now().UTC(), Program: req.Program, Args: req.Args, Dir: req.Dir, Answer: -1}
	resp := CallResponse{Exit: 1}
	w.mu.Lock()
	for i, a := range w.answers {
		if a.call.Program != req.Program || (a.call.Times > 0 && a.used >= a.call.Times) || !matchArgs(a.args, req.Args) {
			continue
		}
		a.used++
		rec.Answer, rec.Answered = i, true
		resp = CallResponse{Stdout: a.call.Respond.Stdout, Stderr: a.call.Respond.Stderr, Exit: a.call.Respond.Exit, Given: true}
		break
	}
	if !rec.Answered && req.Modeled {
		rec.Answered, rec.Modeled = true, true
		resp = CallResponse{}
	}
	handler := w.handlers[req.Program]
	w.mu.Unlock()
	if !rec.Answered && handler != nil {
		// A handler may hold its answer (a slow GitHub): the other fakes
		// are answered meanwhile.
		if got, answered, reason := handler(req); answered {
			resp, rec.Answered, rec.Handler = got, true, true
		} else {
			rec.Reason = reason
		}
	}
	if !rec.Answered {
		resp.Stderr = fmt.Sprintf("scenario world: no given call answers %s\n", rec.Line())
		if rec.Reason != "" {
			resp.Stderr = fmt.Sprintf("scenario world: nothing answers %s: %s\n", rec.Line(), rec.Reason)
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	rec.Seq = len(w.calls) + 1
	rec.Exit = resp.Exit
	w.calls = append(w.calls, rec)
	if w.log != nil {
		if b, err := json.Marshal(rec); err == nil {
			_, _ = w.log.Write(append(b, '\n'))
		}
	}
	return resp
}

// Calls is the log so far.
func (w *World) Calls() []CallRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.calls)
}

// Unanswered are the calls no given call answered.
func (w *World) Unanswered() []CallRecord {
	var out []CallRecord
	for _, c := range w.Calls() {
		if !c.Answered {
			out = append(out, c)
		}
	}
	return out
}

// Count counts the calls of program ("" for all) matching every pattern.
func (w *World) Count(program string, args []string) (int, error) {
	pats, err := compileArgs(args)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range w.Calls() {
		if (program == "" || c.Program == program) && matchArgs(pats, c.Args) {
			n++
		}
	}
	return n, nil
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
				w.serve(conn)
			}()
		}
	}()
	return nil
}

func (w *World) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(time.Minute))
	var req CallRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(CallResponse{Stderr: "scenario world: unreadable call: " + err.Error() + "\n", Exit: 125})
		return
	}
	_ = json.NewEncoder(conn).Encode(w.Answer(req))
}

// Close stops answering and waits for the calls in flight.
func (w *World) Close() error {
	if w.ln == nil {
		return nil
	}
	err := w.ln.Close()
	w.wg.Wait()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// Ask hands one call to the world at socket and returns its answer.
func Ask(socket string, req CallRequest) (CallResponse, error) {
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return CallResponse{}, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(time.Minute))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return CallResponse{}, err
	}
	var resp CallResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return CallResponse{}, err
	}
	return resp, nil
}

// ServeFake runs this process as the fake named by its argv[0] when the
// world's socket is in the environment: it hands the call to the world,
// prints the answer and returns its exit code. served is false for any
// other process.
func ServeFake() (code int, served bool) {
	socket := os.Getenv(WorldEnv)
	name := baseName(os.Args[0])
	if socket == "" || !slices.Contains(Fakes, name) {
		return 0, false
	}
	req := CallRequest{Program: name, Args: os.Args[1:], Stdin: readStdin(2 * time.Second), Env: map[string]string{}}
	req.Dir, _ = os.Getwd()
	for _, k := range FakeEnv {
		if v, ok := os.LookupEnv(k); ok {
			req.Env[k] = v
		}
	}
	resp, err := Ask(socket, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scenario world unreachable for %s: %v\n", name, err)
		return 125, true
	}
	_, _ = io.WriteString(os.Stdout, resp.Stdout)
	_, _ = io.WriteString(os.Stderr, resp.Stderr)
	return resp.Exit, true
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// readStdin reads standard input until EOF or wait: a caller that never
// closes it does not hang the fake.
func readStdin(wait time.Duration) string {
	if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice != 0 {
		return ""
	}
	done := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(os.Stdin, 16<<20))
		done <- b
	}()
	select {
	case b := <-done:
		return string(b)
	case <-time.After(wait):
		return ""
	}
}
