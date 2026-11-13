//go:build scenario

package harness

import (
	"encoding/json"
	"net"
	"os"
	"strings"
	"time"
)

// SocketEnv names the world's harness socket to the fakes. A harness that
// beekeeper starts with an environment of its own choosing (an execution
// host's runner) does not carry it: the fakes fall back to DefaultSocket.
const (
	SocketEnv     = "BEEKEEPER_SCENARIO_HARNESS"
	DefaultSocket = "/work/harness.sock"
	// ClockSocket is the world's clock (internal/clock), which clock.Env
	// names to every process. A harness started with an environment of
	// beekeeper's choosing does not carry that variable either: its fake
	// names the clock again (nameClock).
	ClockSocket = "/work/clock.sock"
	// HostsRoot is where the execution hosts' roots live in the sandbox.
	HostsRoot = "/work/hosts"
)

// The operations a fake asks the world for.
const (
	opCall       = "call"
	opTurn       = "turn"
	opTurnDone   = "turn-done"
	opUnitStart  = "unit-start"
	opUnitVerb   = "unit-verb"
	opUnitState  = "unit-state"
	opUnitList   = "unit-list"
	opBgStart    = "bg-start"
	opBgResume   = "bg-resume"
	opBgList     = "bg-list"
	opBgStop     = "bg-stop"
	opUsage      = "usage"
	opModels     = "models"
	opSSH        = "ssh"
	opSessionPID = "session-pid"
)

// request is one question of a fake to the world.
type request struct {
	Op      string   `json:"op"`
	Program string   `json:"program,omitempty"`
	Args    []string `json:"args,omitempty"`
	Stdin   string   `json:"stdin,omitempty"`
	// Dir and Home place the caller on its machine: the desk, or the host
	// whose root holds them.
	Dir  string `json:"dir,omitempty"`
	Home string `json:"home,omitempty"`
	Exe  string `json:"exe,omitempty"`
	PID  int    `json:"pid,omitempty"`
	// Modeled says the fake answers the call itself unless a given call
	// of the scenario does.
	Modeled bool `json:"modeled,omitempty"`
	// Env is the caller's environment, for what the world starts in its
	// place: a background session.
	Env []string `json:"env,omitempty"`

	Unit    *unitSpec  `json:"unit,omitempty"`
	Verb    string     `json:"verb,omitempty"`
	Names   []string   `json:"names,omitempty"`
	States  []string   `json:"states,omitempty"`
	Session *sessionID `json:"session,omitempty"`
	Report  *report    `json:"report,omitempty"`
	Harness string     `json:"harness,omitempty"`
	Dest    string     `json:"dest,omitempty"`
	All     bool       `json:"all,omitempty"`
}

// reply is the world's answer.
type reply struct {
	// Error is why the world could not answer.
	Error string `json:"error,omitempty"`
	// Given is a given call's answer of the scenario: the fake prints it
	// and exits with it instead of answering itself.
	Given *canned `json:"given,omitempty"`
	// Machine is the machine the caller is on.
	Machine string    `json:"machine,omitempty"`
	Turn    *turnPlan `json:"turn,omitempty"`
	State   string    `json:"state,omitempty"`
	Lines   []string  `json:"lines,omitempty"`
	Exit    int       `json:"exit,omitempty"`
	Message string    `json:"message,omitempty"`
	Rows    []bgRow   `json:"rows,omitempty"`
	Short   string    `json:"short,omitempty"`
	Usage   *Usage    `json:"usage,omitempty"`
	Full    bool      `json:"full,omitempty"`
	Models  []string  `json:"models,omitempty"`
	SSH     *sshRoute `json:"ssh,omitempty"`

	// Fail is the account's refusal of this usage request.
	Fail *UsageFail `json:"fail,omitempty"`
}

// canned is a given call's answer.
type canned struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Exit   int    `json:"exit"`
}

// unitSpec is a transient unit systemd-run asks for.
type unitSpec struct {
	Name     string   `json:"name"`
	Dir      string   `json:"dir,omitempty"`
	Argv     []string `json:"argv"`
	Env      []string `json:"env,omitempty"`
	Collect  bool     `json:"collect,omitempty"`
	KillMode string   `json:"killMode,omitempty"`
	// Success are the exit statuses and signal names that count as a
	// clean end beside 0 (SuccessExitStatus).
	Success     []string `json:"success,omitempty"`
	StopPost    string   `json:"stopPost,omitempty"`
	StopTimeout int      `json:"stopTimeout,omitempty"`
	MaxRuntime  int      `json:"maxRuntime,omitempty"`
}

// sessionID names a worker session to the world.
type sessionID struct {
	Harness string `json:"harness"`
	Name    string `json:"name"`
	ID      string `json:"id"`
	Prompt  string `json:"prompt,omitempty"`
	Model   string `json:"model,omitempty"`
}

// turnPlan is what a session does in one turn.
type turnPlan struct {
	// Index is the turn's number for its worker, from 0.
	Index   int               `json:"index"`
	Steps   []Step            `json:"steps,omitempty"`
	Outcome string            `json:"outcome"`
	Exit    int               `json:"exit,omitempty"`
	Tokens  Tokens            `json:"tokens"`
	Reply   string            `json:"reply"`
	Vars    map[string]string `json:"vars"`
}

// report is a session's account of a turn.
type report struct {
	Index   int          `json:"index"`
	Steps   []stepResult `json:"steps,omitempty"`
	Outcome string       `json:"outcome"`
	// Failure is why the turn's script did not run to its end.
	Failure string `json:"failure,omitempty"`
}

// stepResult is one step as it ran.
type stepResult struct {
	Line   string `json:"line"`
	Exit   int    `json:"exit"`
	Output string `json:"output,omitempty"`
}

// bgRow is one row of claude agents --json.
type bgRow struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	PID       int    `json:"pid"`
	Status    string `json:"status,omitempty"`
}

// sshRoute is where an ssh destination leads.
type sshRoute struct {
	Root  string   `json:"root"`
	Home  string   `json:"home"`
	Env   []string `json:"env"`
	Fault string   `json:"fault,omitempty"`
	Slow  int64    `json:"slowMs,omitempty"`
}

// socketPath is the world's socket as this process finds it; "" when the
// process runs outside a scenario.
func socketPath() string {
	if s := os.Getenv(SocketEnv); s != "" {
		return s
	}
	if fi, err := os.Stat(DefaultSocket); err == nil && fi.Mode()&os.ModeSocket != 0 {
		return DefaultSocket
	}
	return ""
}

// ask sends one request to the world and returns its reply.
func ask(socket string, req request) (reply, error) {
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return reply{}, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(time.Minute))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return reply{}, err
	}
	var rep reply
	if err := json.NewDecoder(conn).Decode(&rep); err != nil {
		return reply{}, err
	}
	return rep, nil
}

// MachineOf is the machine a path is on: the host whose root holds it,
// else the desk (LocalHost).
func MachineOf(paths ...string) string {
	for _, p := range paths {
		if rest, ok := strings.CutPrefix(p, HostsRoot+"/"); ok {
			name, _, _ := strings.Cut(rest, "/")
			if name != "" {
				return name
			}
		}
	}
	return LocalHost
}
