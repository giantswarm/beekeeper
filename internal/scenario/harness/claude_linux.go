//go:build scenario && linux

package harness

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The fake claude speaks the parts of Claude Code beekeeper uses:
//
//   - claude --bg … -- <brief> starts a background session: the claude
//     daemon (a claude process the world keeps) runs the session's CLI
//     under a terminal host, as the real daemon does, so beekeeper finds
//     the session in the process table. claude --bg --resume <id> <msg>
//     continues a stopped one, claude agents --json lists them and claude
//     stop ends one.
//   - claude -p … -- <prompt> is a headless turn: the CLI runs one turn
//     and ends.
//   - claude --print --input-format stream-json … is an execution host's
//     managed session: user messages on standard input, the stream on
//     standard output.
//   - claude -p /usage, plugin list, auth status and --version answer
//     beekeeper's probes.
//
// A session's CLI keeps the record and the transcript the real one keeps
// (~/.claude/sessions/<pid>.json, ~/.claude/projects/<folder>/<id>.jsonl)
// and runs its worker script as its tool commands.

const (
	claudeFlagBg      = "--bg"
	claudeFlagResume  = "--resume"
	claudeFlagSession = "--session-id"
	ptyHostCmd        = "bg-pty-host"
	daemonCmd         = "daemon"
)

// claudePrint says whether args ask for a headless turn, and its prompt.
func claudePrint(args []string) (prompt string, ok bool) {
	if !slices.Contains(args, "-p") && !slices.Contains(args, "--print") {
		return "", false
	}
	if v := argValue(args, "-p", "--print"); v != "" && !strings.HasPrefix(v, "-") {
		return v, true
	}
	return lastPrompt(args), true
}

// lastPrompt is the prompt a claude command line ends with: what follows
// "--", else its last argument.
func lastPrompt(args []string) string {
	if i := slices.Index(args, "--"); i >= 0 {
		return strings.Join(args[i+1:], " ")
	}
	if len(args) == 0 {
		return ""
	}
	return args[len(args)-1]
}

func claudeModeled(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch {
	case slices.Contains(args, claudeFlagBg):
		return true
	case slices.Contains(args, "--version") || slices.Contains(args, "-v"):
		return true
	case args[0] == "agents" && slices.Contains(args, "--json"):
		return true
	case args[0] == "stop" && len(args) == 2:
		return true
	case len(args) >= 2 && args[0] == "plugin" && args[1] == "list":
		return true
	case len(args) == 2 && args[0] == "auth" && args[1] == "status":
		return true
	}
	_, print := claudePrint(args)
	return print
}

func claude(c *client) int {
	args := c.args
	switch {
	case slices.Contains(args, "--version") || slices.Contains(args, "-v"):
		fmt.Println(ClaudeVersion + " (Claude Code)")
		return 0
	case slices.Contains(args, claudeFlagBg) && slices.Contains(args, claudeFlagResume):
		rep, err := c.ask(request{Op: opBgResume, Env: os.Environ()})
		return c.backgrounded(rep, err)
	case slices.Contains(args, claudeFlagBg):
		rep, err := c.ask(request{Op: opBgStart, Env: os.Environ()})
		return c.backgrounded(rep, err)
	case args[0] == "agents":
		rep, err := c.ask(request{Op: opBgList, All: slices.Contains(args, "--all")})
		if err != nil {
			return c.end(rep, err)
		}
		rows := rep.Rows
		if rows == nil {
			rows = []bgRow{}
		}
		return printJSON(rows)
	case args[0] == "stop":
		rep, err := c.ask(request{Op: opBgStop, Names: []string{args[1]}})
		return c.end(rep, err)
	case args[0] == "plugin":
		fmt.Println("[]")
		return 0
	case args[0] == "auth":
		return printJSON(map[string]any{"loggedIn": true, "authMethod": "claude.ai", "apiProvider": "firstParty", "configDirectory": claudeConfigDir(), "subscriptionType": "max"})
	}
	prompt, _ := claudePrint(args)
	if strings.TrimSpace(prompt) == "/usage" {
		return claudeUsage(c)
	}
	return claudeSession(c, false)
}

func printJSON(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(string(b))
	return 0
}

// backgrounded prints the line claude --bg prints for the session it
// started or continued.
func (c *client) backgrounded(rep reply, err error) int {
	if err != nil || rep.Exit != 0 {
		return c.end(rep, err)
	}
	fmt.Printf("backgrounded · %s · %s\n", rep.Short, rep.Message)
	return 0
}

// claudeUsage answers claude -p /usage --output-format json as the CLI
// does: the session and week windows in one result text.
func claudeUsage(c *client) int {
	rep, err := c.ask(request{Op: opUsage, Harness: Claude})
	if err != nil || rep.Usage == nil {
		return c.end(rep, err)
	}
	now := time.Now().UTC()
	const stamp = "Jan 2, 3:04pm"
	text := fmt.Sprintf("Current session: %.0f%% used · resets %s (UTC)\nCurrent week (all models): %.0f%% used · resets %s (UTC)",
		rep.Usage.Session, now.Add(2*time.Hour).Format(stamp), rep.Usage.Week, now.Add(72*time.Hour).Format(stamp))
	if slices.Contains(c.args, "json") || argValue(c.args, "--output-format") == "json" {
		return printJSON(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text})
	}
	fmt.Println(text)
	return 0
}

func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	return filepath.Join(os.Getenv("HOME"), ".claude")
}

// projectFolder is the folder of a directory's transcripts, as Claude Code
// names it.
func projectFolder(dir string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, dir)
}

func transcriptPath(configDir, dir, id string) string {
	return filepath.Join(configDir, "projects", projectFolder(dir), id+".jsonl")
}

// newUUID is a random version 4 UUID.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// claudeModel is the model a session's replies name.
func claudeModel(flag string) string {
	switch {
	case flag == "":
		return "claude-sonnet-4-5"
	case strings.HasPrefix(flag, "claude-"):
		return flag
	}
	return "claude-" + flag + "-4-5"
}

// transcript is a session's transcript file.
type transcript struct {
	path, id, dir string
	parent        string
}

func (t *transcript) write(line map[string]any) {
	uuid := newUUID()
	if _, ok := line["uuid"]; !ok && line["type"] != "custom-title" {
		line["uuid"] = uuid
		line["parentUuid"] = nilIfEmpty(t.parent)
		line["timestamp"] = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		line["cwd"] = t.dir
		line["version"] = ClaudeVersion
		t.parent = uuid
	}
	line["sessionId"] = t.id
	b, err := json.Marshal(line)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(t.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // the session's transcript under the scenario's home
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (t *transcript) user(text string) {
	t.write(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
}

func (t *transcript) assistant(model string, tokens Tokens, content ...map[string]any) {
	t.write(map[string]any{"type": "assistant", "message": map[string]any{
		"id": "msg_" + strings.ReplaceAll(newUUID(), "-", "")[:24], "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": "end_turn",
		"usage": map[string]any{"input_tokens": tokens.Input, "output_tokens": tokens.Output, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0},
	}})
}

// tool writes one Bash tool call and its result.
func (t *transcript) tool(model string, r stepResult) {
	id := "toolu_" + strings.ReplaceAll(newUUID(), "-", "")[:24]
	t.assistant(model, Tokens{}, map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": r.Line}})
	t.write(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]any{
		{"type": "tool_result", "tool_use_id": id, "content": r.Output, "is_error": r.Exit != 0},
	}}})
}

// limitText is what Claude Code says when the subscription's window is
// used up.
const limitText = "You've hit your limit · resets 3pm (UTC)"

// claudeSession runs this process as a session's CLI: a background
// session's (daemon), which lives on between its turns, a headless turn's,
// which ends with its turn, or an execution host's managed session.
func claudeSession(c *client, daemon bool) int {
	args := c.args
	dir, _ := os.Getwd()
	id, resumed := argValue(args, claudeFlagSession), false
	if id == "" {
		if v := argValue(args, claudeFlagResume, "-r"); v != "" {
			id, resumed = strings.TrimSuffix(filepath.Base(v), ".jsonl"), true
		}
	}
	if id == "" {
		id = newUUID()
	}
	name := argValue(args, "-n", "--name")
	model := claudeModel(argValue(args, "--model"))
	stream := argValue(args, "--input-format") == "stream-json"
	streamOut := argValue(args, "--output-format") == "stream-json"
	tr := &transcript{path: transcriptPath(claudeConfigDir(), dir, id), id: id, dir: dir}
	s := sessionID{Harness: Claude, Name: name, ID: id, Model: model}
	writeCLIRecord(id, name, dir, daemon)
	if !resumed && name != "" {
		tr.write(map[string]any{"type": "custom-title", "customTitle": name})
	}
	env := append(os.Environ(), "CLAUDE_CODE_SESSION_ID="+id, "CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=cli")
	out := json.NewEncoder(os.Stdout)
	emit := func(v map[string]any) {
		if streamOut {
			v["session_id"] = id
			_ = out.Encode(v)
		}
	}
	emit(map[string]any{"type": "system", "subtype": "init", "cwd": dir, "model": model, "permissionMode": argValue(args, "--permission-mode")})

	// turn runs one turn on prompt; it returns the turn's exit code and
	// whether the CLI goes on afterwards.
	turn := func(prompt string) (code int, on bool) {
		s.Prompt = prompt
		plan := c.beginTurn(s)
		if plan == nil {
			return 125, false
		}
		tr.user(prompt)
		results, failure := runSteps(plan, dir, env, func(_ string, r stepResult) {
			tr.tool(model, r)
			emit(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "model": model, "content": []map[string]any{{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": r.Line}}}}})
		})
		c.finishTurn(s, plan, results, failure)
		switch plan.Outcome {
		case Crash:
			return plan.Exit, false
		case Hang:
			forever()
		case Budget:
			tr.write(map[string]any{"type": "assistant", "isApiErrorMessage": true, "error": "rate_limit", "message": map[string]any{
				"id": "msg_limit", "type": "message", "role": "assistant", "model": "<synthetic>",
				"content": []map[string]any{{"type": "text", "text": limitText}},
				"usage":   map[string]any{"input_tokens": 0, "output_tokens": 0},
			}})
			emit(map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": limitText, "num_turns": 1})
			if !streamOut && !daemon {
				fmt.Println(limitText)
			}
			return 1, daemon
		}
		tr.assistant(model, plan.Tokens, map[string]any{"type": "text", "text": plan.Reply})
		emit(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "model": model, "content": []map[string]any{{"type": "text", "text": plan.Reply}},
			"usage": map[string]any{"input_tokens": plan.Tokens.Input, "output_tokens": plan.Tokens.Output}}})
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": plan.Reply, "num_turns": 1,
			"usage": map[string]any{"input_tokens": plan.Tokens.Input, "output_tokens": plan.Tokens.Output}})
		if !streamOut && !daemon {
			if argValue(args, "--output-format") == "json" {
				printJSON(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": plan.Reply, "session_id": id})
			} else {
				fmt.Println(plan.Reply)
			}
		}
		return 0, true
	}

	if stream {
		// An execution host's managed session: one turn per user message on
		// standard input, each replayed on the stream as its acceptance.
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			var msg struct {
				Type    string `json:"type"`
				UUID    string `json:"uuid"`
				Message struct {
					Content any `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.Type != "user" {
				continue
			}
			text, _ := msg.Message.Content.(string)
			emit(map[string]any{"type": "user", "uuid": msg.UUID, "isReplay": true, "parent_tool_use_id": nil, "message": map[string]any{"role": "user", "content": text}})
			if code, on := turn(text); !on {
				return code
			}
		}
		return 0
	}
	prompt, _ := claudePrint(args)
	if daemon {
		prompt = lastPrompt(args)
	}
	code, on := turn(prompt)
	if !daemon || !on {
		return code
	}
	// A background session's CLI lives on between its turns, until claude
	// stop or the daemon's end.
	untilSignal()
	return 0
}

// writeCLIRecord keeps the record a running CLI keeps of its session.
func writeCLIRecord(id, name, dir string, daemon bool) {
	rec := map[string]any{
		"pid": os.Getpid(), "sessionId": id, "cwd": dir, "startedAt": time.Now().UnixMilli(), "procStart": startTicks(),
		"version": ClaudeVersion, "entrypoint": "cli", "name": name, "status": "busy", "updatedAt": time.Now().UnixMilli(),
	}
	if daemon {
		rec["kind"] = "bg"
		rec["jobId"] = id[:min(8, len(id))]
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	p := filepath.Join(claudeConfigDir(), "sessions", fmt.Sprintf("%d.json", os.Getpid()))
	if os.MkdirAll(filepath.Dir(p), 0o700) == nil {
		_ = os.WriteFile(p, b, 0o600)
	}
}

// claudeSpawned runs this process as a part of the fake claude itself:
// the daemon, a session's terminal host, or the CLI under it.
func claudeSpawned(c *client) int {
	switch {
	case len(c.args) > 0 && c.args[0] == daemonCmd:
		return claudeDaemon()
	case len(c.args) > 0 && c.args[0] == ptyHostCmd:
		return claudePtyHost(c)
	}
	_, _ = c.ask(request{Op: opSessionPID, Session: &sessionID{Harness: Claude, ID: sessionArg(c.args)}})
	return claudeSession(c, true)
}

func sessionArg(args []string) string {
	if id := argValue(args, claudeFlagSession); id != "" {
		return id
	}
	return strings.TrimSuffix(filepath.Base(argValue(args, claudeFlagResume, "-r")), ".jsonl")
}

// daemonOrder is one order of the world to a claude daemon; daemonEvent
// what the daemon tells it back.
type daemonOrder struct {
	Op   string   `json:"op"`
	Job  string   `json:"job"`
	Argv []string `json:"argv,omitempty"`
	Dir  string   `json:"dir,omitempty"`
	Env  []string `json:"env,omitempty"`
}

type daemonEvent struct {
	Event string `json:"event"`
	Job   string `json:"job"`
	PID   int    `json:"pid,omitempty"`
	Code  int    `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// claudeDaemon runs this process as the claude daemon: it starts each
// background session the world orders under a terminal host of its own and
// says when one ended. The world's end of its standard input is its end.
func claudeDaemon() int {
	var mu sync.Mutex
	jobs := map[string]*exec.Cmd{}
	out := json.NewEncoder(os.Stdout)
	say := func(e daemonEvent) {
		mu.Lock()
		defer mu.Unlock()
		_ = out.Encode(e)
	}
	signal.Ignore(syscall.SIGHUP)
	dec := json.NewDecoder(os.Stdin)
	for {
		var o daemonOrder
		if err := dec.Decode(&o); err != nil {
			break
		}
		switch o.Op {
		case "spawn":
			if len(o.Argv) == 0 {
				say(daemonEvent{Event: "exited", Job: o.Job, Code: 127, Error: "no command"})
				continue
			}
			bin := o.Argv[0]
			sock := filepath.Join(os.TempDir(), "cc-daemon-0", "00000000", "pty", o.Job+".sock")
			cmd := exec.Command(bin, append([]string{ptyHostCmd, "--bg-pty-host", sock, "200", "50", "--"}, o.Argv...)...) //nolint:gosec // the session the scenario's world ordered
			cmd.Args[0] = Claude
			cmd.Dir = o.Dir
			cmd.Env = append(slices.Clone(o.Env), spawnedEnv+"=1")
			cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
			if err := cmd.Start(); err != nil {
				say(daemonEvent{Event: "exited", Job: o.Job, Code: 127, Error: err.Error()})
				continue
			}
			mu.Lock()
			jobs[o.Job] = cmd
			mu.Unlock()
			say(daemonEvent{Event: "started", Job: o.Job, PID: cmd.Process.Pid})
			go func() {
				err := cmd.Wait()
				code := 0
				if err != nil {
					code = 1
					if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() >= 0 {
						code = cmd.ProcessState.ExitCode()
					}
				}
				mu.Lock()
				if jobs[o.Job] == cmd {
					delete(jobs, o.Job)
				}
				mu.Unlock()
				say(daemonEvent{Event: "exited", Job: o.Job, Code: code})
			}()
		case "kill":
			mu.Lock()
			cmd := jobs[o.Job]
			mu.Unlock()
			if cmd != nil {
				_ = cmd.Process.Signal(syscall.SIGTERM)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, cmd := range jobs {
		_ = cmd.Process.Kill()
	}
	return 0
}

// claudePtyHost runs this process as a background session's terminal
// host: the session's CLI is its child, and its end is the host's.
func claudePtyHost(c *client) int {
	i := slices.Index(c.args, "--")
	if i < 0 || i+1 >= len(c.args) {
		fmt.Fprintln(os.Stderr, "claude bg-pty-host: no command")
		return 127
	}
	argv := c.args[i+1:]
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // the session's CLI
	cmd.Env = append(os.Environ(), spawnedEnv+"=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "claude bg-pty-host:", err)
		return 127
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for s := range sig {
			_ = cmd.Process.Signal(s)
		}
	}()
	if err := cmd.Wait(); err != nil {
		if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() >= 0 {
			return cmd.ProcessState.ExitCode()
		}
		return 1
	}
	return 0
}
