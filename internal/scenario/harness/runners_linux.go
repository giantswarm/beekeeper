//go:build scenario && linux

package harness

import (
	"bufio"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

// The fake codex, agy and omp: the CLIs beekeeper drives through a runner
// in a transient unit. Each speaks its stream protocol as far as the
// runner reads it and runs its worker script as its tool commands, under
// the identity the unit's environment gives it.

// agentName is the name of the worker a runner's harness serves.
const agentName = "BEEKEEPER_AGENT_NAME"

// stream writes JSON lines to standard output, one event each.
type stream struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func newStream() *stream { return &stream{enc: json.NewEncoder(os.Stdout)} }

func (s *stream) emit(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.enc.Encode(v)
}

// The fake codex.
//
//   - codex exec --json … - runs one turn on the prompt of its standard
//     input in a new thread; codex exec resume --json … <thread> - one more
//     in that thread. The stream names the thread, the turn's start, its
//     items and its end with the tokens it used.
//   - codex app-server answers the account's rate limits, the usage probe.
//   - codex --version and codex login status answer a host's probes.

func codexModeled(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "exec", "app-server":
		return true
	case "--version":
		return len(args) == 1
	case "login":
		return len(args) == 2 && args[1] == "status"
	}
	return false
}

// codexLimit is what codex says when the account's window is used up.
const codexLimit = "You've hit your usage limit. Upgrade to Pro or try again in 2 hours."

func codex(c *client) int {
	args := c.args
	switch args[0] {
	case "--version":
		fmt.Println("codex-cli " + CodexVersion)
		return 0
	case "login":
		// The real CLI reports its login on standard error.
		fmt.Fprintln(os.Stderr, "Logged in using ChatGPT")
		return 0
	case "app-server":
		return codexAppServer(c)
	}
	resume := len(args) > 1 && args[1] == "resume"
	thread := newUUID()
	if resume {
		// exec resume … <thread> -
		for i := len(args) - 1; i > 1; i-- {
			if a := args[i]; a != "-" && !strings.HasPrefix(a, "-") && (i == 2 || !slices.Contains([]string{"-m", "-c", "--model"}, args[i-1])) {
				thread = a
				break
			}
		}
	}
	prompt, _ := io.ReadAll(io.LimitReader(os.Stdin, 16<<20))
	out := newStream()
	out.emit(map[string]any{"type": "thread.started", "thread_id": thread})
	out.emit(map[string]any{"type": "turn.started"})
	s := sessionID{Harness: Codex, Name: os.Getenv(agentName), ID: thread, Prompt: string(prompt), Model: argValue(args, "-m", "--model")}
	plan := c.beginTurn(s)
	if plan == nil {
		return 125
	}
	dir, _ := os.Getwd()
	n := 0
	results, failure := runSteps(plan, dir, append(os.Environ(), "CODEX_THREAD_ID="+thread), func(_ string, r stepResult) {
		id := fmt.Sprintf("item_%d", n)
		n++
		out.emit(map[string]any{"type": "item.started", "item": map[string]any{"id": id, "type": "command_execution", "command": r.Line, "status": "in_progress"}})
		out.emit(map[string]any{"type": "item.completed", "item": map[string]any{"id": id, "type": "command_execution", "command": r.Line, "aggregated_output": r.Output, "exit_code": r.Exit, "status": "completed"}})
	})
	c.finishTurn(s, plan, results, failure)
	switch plan.Outcome {
	case Crash:
		return plan.Exit
	case Hang:
		forever()
	case Budget:
		out.emit(map[string]any{"type": "error", "message": codexLimit})
		out.emit(map[string]any{"type": "turn.failed", "error": map[string]any{"message": codexLimit}})
		return 1
	}
	out.emit(map[string]any{"type": "item.completed", "item": map[string]any{"id": fmt.Sprintf("item_%d", n), "type": "agent_message", "text": plan.Reply}})
	out.emit(map[string]any{"type": "turn.completed", "usage": map[string]any{"input_tokens": plan.Tokens.Input, "cached_input_tokens": 0, "output_tokens": plan.Tokens.Output}})
	return 0
}

// codexAppServer answers the two requests of beekeeper's usage probe and
// ends with its standard input.
func codexAppServer(c *client) int {
	out := newStream()
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var msg struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.ID == nil {
			continue
		}
		switch msg.Method {
		case "initialize":
			out.emit(map[string]any{"id": *msg.ID, "result": map[string]any{"userAgent": "codex-cli/" + CodexVersion}})
		case "account/rateLimits/read":
			rep, err := c.ask(request{Op: opUsage, Harness: Codex})
			if err != nil || rep.Usage == nil {
				out.emit(map[string]any{"id": *msg.ID, "error": map[string]any{"code": -32000, "message": "usage unavailable"}})
				continue
			}
			if f := rep.Fail; f != nil && f.Hang {
				holdOutput()
				forever()
			}
			if f := rep.Fail; f != nil {
				out.emit(map[string]any{"id": *msg.ID, "error": map[string]any{"code": cmp.Or(f.Code, -32603), "message": f.Message}})
				continue
			}
			now := time.Now()
			out.emit(map[string]any{"id": *msg.ID, "result": map[string]any{"rateLimits": map[string]any{
				"primary":          map[string]any{"usedPercent": rep.Usage.Session, "windowDurationMins": 300, "resetsAt": now.Add(2 * time.Hour).Unix()},
				"secondary":        map[string]any{"usedPercent": rep.Usage.Week, "windowDurationMins": 10080, "resetsAt": now.Add(72 * time.Hour).Unix()},
				"rateLimitReached": rep.Full,
			}}})
		default:
			out.emit(map[string]any{"id": *msg.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		}
	}
	return 0
}

// holdOutput starts a helper that keeps the app-server's standard output
// open after the app-server ends, as a helper of the real one can.
func holdOutput() {
	for _, sleep := range []string{"/usr/bin/sleep", "/bin/sleep"} {
		if _, err := os.Stat(sleep); err == nil {
			c := exec.Command(sleep, "3600") //nolint:gosec // a fixed system program
			c.Stdout = os.Stdout
			_ = c.Start()
			return
		}
	}
}

// The fake agy.
//
//   - agy --input-format stream-json --output-format stream-json … is a
//     session: an init event names its conversation, each user message on
//     standard input is a turn, accepted with a user_input step and ended
//     with a result.
//   - agy models lists the models, agy --version its version.
//   - agy -p /usage is the usage probe: the Gemini models' weekly and
//     five-hour windows, as harness.usage.agy gives them.

func agyModeled(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "models" || len(args) == 1 && args[0] == "--version" || agyUsageProbe(args) {
		return true
	}
	return argValue(args, "--input-format") == "stream-json"
}

// agyUsageProbe reports whether args are agy's usage probe, agy -p /usage.
func agyUsageProbe(args []string) bool {
	return len(args) == 2 && args[0] == "-p" && strings.TrimSpace(args[1]) == "/usage"
}

// agyQuota is the error agy reports when the account's quota is used up.
const agyQuota = `AGY_ERROR: {"short_error":"Individual quota reached. Resets in 2h0m0s.","error_code":429,"status":"RESOURCE_EXHAUSTED","retryable":false}`

func agy(c *client) int {
	args := c.args
	switch {
	case args[0] == "--version":
		fmt.Println(AgyVersion)
		return 0
	case args[0] == "models":
		rep, err := c.ask(request{Op: opModels, Harness: Agy})
		if err != nil {
			return c.end(rep, err)
		}
		for _, m := range rep.Models {
			fmt.Println(m)
		}
		return 0
	case agyUsageProbe(args):
		return agyUsage(c)
	}
	conv := argValue(args, "--conversation")
	if conv == "" {
		conv = newUUID()
	}
	out := newStream()
	out.emit(map[string]any{"event": "init", "conversation_id": conv, "model": argValue(args, "--model")})
	dir, _ := os.Getwd()
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var msg struct {
			Event   string `json:"event"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.Event != "user" {
			continue
		}
		out.emit(map[string]any{"event": "step_update", "conversation_id": conv, "step_update": map[string]any{"conversation_id": conv, "step_type": "user_input", "status": "done"}})
		s := sessionID{Harness: Agy, Name: os.Getenv(agentName), ID: conv, Prompt: msg.Message.Content, Model: argValue(args, "--model")}
		plan := c.beginTurn(s)
		if plan == nil {
			return 125
		}
		results, failure := runSteps(plan, dir, os.Environ(), func(_ string, r stepResult) {
			out.emit(map[string]any{"event": "step_update", "conversation_id": conv, "step_update": map[string]any{"conversation_id": conv, "step_type": "run_command", "status": "done", "command": r.Line, "exit_code": r.Exit}})
		})
		c.finishTurn(s, plan, results, failure)
		switch plan.Outcome {
		case Crash:
			return plan.Exit
		case Hang:
			forever()
		case Budget:
			// agy reports the quota and stays alive on its standard input.
			fmt.Fprintln(os.Stderr, agyQuota)
			continue
		}
		out.emit(map[string]any{"event": "step_update", "conversation_id": conv, "step_update": map[string]any{"conversation_id": conv, "step_type": "planner_response", "status": "done", "text": plan.Reply}})
		out.emit(map[string]any{"event": "result", "conversation_id": conv, "result": map[string]any{"conversation_id": conv, "status": "ok",
			"usage": map[string]any{"input_tokens": plan.Tokens.Input, "output_tokens": plan.Tokens.Output}}})
	}
	return 0
}

// agyUsage answers agy -p /usage as the CLI does: per model bucket, the
// remaining share of each window and its reset, tab-separated. The week
// resets in 72 hours and the five hours in 2, as the claude and codex
// probes say.
func agyUsage(c *client) int {
	rep, err := c.ask(request{Op: opUsage, Harness: Agy})
	if err != nil || rep.Usage == nil {
		return c.end(rep, err)
	}
	now := time.Now().UTC()
	fmt.Printf("Gemini Models\tWeekly Limit Remaining\t%.0f%%\t%s\n", 100-rep.Usage.Week, now.Add(72*time.Hour).Format(time.RFC3339))
	fmt.Printf("Gemini Models\tFive Hour Limit Remaining\t%.0f%%\t%s\n", 100-rep.Usage.Session, now.Add(2*time.Hour).Format(time.RFC3339))
	return 0
}

// The fake omp.
//
//   - omp --mode rpc … is a session: each steer command on standard input
//     (its inbox) is a turn.
//   - omp models --json lists the models, omp --version its version.

func ompModeled(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "models" || len(args) == 1 && args[0] == "--version" {
		return true
	}
	return argValue(args, "--mode") == "rpc"
}

func omp(c *client) int {
	args := c.args
	switch {
	case args[0] == "--version":
		fmt.Println("omp/" + OmpVersion)
		return 0
	case args[0] == "models":
		rep, err := c.ask(request{Op: opModels, Harness: Omp})
		if err != nil {
			return c.end(rep, err)
		}
		type model struct {
			Selector string `json:"selector"`
		}
		models := make([]model, 0, len(rep.Models))
		for _, m := range rep.Models {
			models = append(models, model{m})
		}
		return printJSON(map[string]any{"models": models})
	}
	id := newUUID()
	out := newStream()
	out.emit(map[string]any{"type": "ready", "sessionId": id})
	dir, _ := os.Getwd()
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var cmd struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &cmd) != nil || cmd.Type != "steer" && cmd.Type != "prompt" {
			continue
		}
		out.emit(map[string]any{"type": "agent_start"})
		s := sessionID{Harness: Omp, Name: os.Getenv(agentName), ID: id, Prompt: cmd.Message, Model: argValue(args, "--model")}
		plan := c.beginTurn(s)
		if plan == nil {
			return 125
		}
		results, failure := runSteps(plan, dir, os.Environ(), nil)
		c.finishTurn(s, plan, results, failure)
		switch plan.Outcome {
		case Crash:
			return plan.Exit
		case Hang:
			forever()
		case Budget:
			out.emit(map[string]any{"type": "error", "message": "the model's endpoint refused the request: 429"})
			continue
		}
		out.emit(map[string]any{"type": "agent_end", "text": plan.Reply})
	}
	return 0
}
