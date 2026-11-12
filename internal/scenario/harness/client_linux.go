//go:build scenario && linux

package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// spawnedEnv marks a process the world or one of its fakes started as part
// of a fake itself (a claude daemon, a session's CLI): it is no call of
// beekeeper's and is not logged as one. The process takes the mark off its
// own environment, so nothing it starts inherits it.
const spawnedEnv = "BEEKEEPER_SCENARIO_SPAWNED"

// The versions the fake CLIs report, each inside the range beekeeper's
// execution adapter supports.
const (
	ClaudeVersion = "2.1.300"
	CodexVersion  = "0.170.0"
	AgyVersion    = "1.5.0"
	OmpVersion    = "18.15.0"
)

// client is this process as a fake.
type client struct {
	name    string
	args    []string
	socket  string
	exe     string
	spawned bool
}

// Main runs this process as the fake its name says, when it is one of the
// package's and the scenario's world is reachable. served is false for
// any other process.
func Main() (code int, served bool) {
	name := filepath.Base(os.Args[0])
	if name == "git" || slices.Contains(RemoteHelpers, name) {
		if _, err := os.Stat(RealGit); err != nil {
			return 0, false
		}
		if name == "git" {
			return git(), true
		}
		return gitRemote(), true
	}
	hostTool := name == "go" || name == "gh"
	if !slices.Contains(Programs, name) && !hostTool {
		return 0, false
	}
	socket := socketPath()
	if socket == "" {
		return 0, false
	}
	c := &client{name: name, args: os.Args[1:], socket: socket, exe: invokedPath(), spawned: os.Getenv(spawnedEnv) != ""}
	_ = os.Unsetenv(spawnedEnv)
	if name == "gh" {
		// Only a host's pinned gh is the package's, and of it only what the
		// host probes; the scenario's world answers every other gh call.
		if MachineOf(c.exe) == LocalHost || !ghModeled(c.args) {
			return 0, false
		}
	}
	return c.run(), true
}

// invokedPath is the path this process was started under, the fake's
// name and not the executable every fake links to.
func invokedPath() string {
	p := os.Args[0]
	if !strings.Contains(p, "/") {
		if found, err := exec.LookPath(p); err == nil {
			p = found
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return p
}

func (c *client) run() int {
	if c.spawned {
		switch c.name {
		case Claude:
			return claudeSpawned(c)
		}
	}
	modeled := false
	switch c.name {
	case Claude:
		modeled = claudeModeled(c.args)
	case Codex:
		modeled = codexModeled(c.args)
	case Agy:
		modeled = agyModeled(c.args)
	case Omp:
		modeled = ompModeled(c.args)
	case "systemd-run":
		modeled = true
	case "systemctl":
		modeled = systemctlModeled(c.args)
	case "ssh":
		modeled = true
	case "go":
		modeled = len(c.args) == 1 && c.args[0] == "version"
	case "gh":
		modeled = true
	}
	stdin := ""
	if !modeled {
		stdin = readStdin(2 * time.Second)
	}
	rep, err := c.ask(request{Op: opCall, Modeled: modeled, Stdin: stdin})
	if err != nil {
		fmt.Fprintf(os.Stderr, "scenario world unreachable for %s: %v\n", c.name, err)
		return 125
	}
	if rep.Error != "" {
		fmt.Fprintf(os.Stderr, "scenario world: %s\n", rep.Error)
		return 125
	}
	if g := rep.Given; g != nil {
		_, _ = io.WriteString(os.Stdout, g.Stdout)
		_, _ = io.WriteString(os.Stderr, g.Stderr)
		return g.Exit
	}
	switch c.name {
	case Claude:
		return claude(c)
	case Codex:
		return codex(c)
	case Agy:
		return agy(c)
	case Omp:
		return omp(c)
	case "systemd-run":
		return systemdRun(c)
	case "systemctl":
		return systemctl(c)
	case "ssh":
		return ssh(c)
	case "go":
		fmt.Printf("go version go%s linux/amd64\n", toolVersion(c.exe))
		return 0
	case "gh":
		return hostGh(c)
	}
	return 1
}

// ask sends a request under this fake's name, arguments and place.
func (c *client) ask(req request) (reply, error) {
	if req.Program == "" {
		req.Program = c.name
	}
	if req.Args == nil {
		req.Args = c.args
	}
	if req.Dir == "" {
		req.Dir, _ = os.Getwd()
	}
	req.Home, req.Exe, req.PID = os.Getenv("HOME"), c.exe, os.Getpid()
	rep, err := ask(c.socket, req)
	if err != nil {
		return rep, fmt.Errorf("%w: %v", errNoWorld, err)
	}
	if rep.Error != "" {
		return rep, errors.New(rep.Error)
	}
	return rep, nil
}

// end prints a reply's message, or the error, and returns the exit code.
func (c *client) end(rep reply, err error) int {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", c.name, err)
		return 125
	}
	if rep.Message != "" {
		w := os.Stdout
		if rep.Exit != 0 {
			w = os.Stderr
		}
		_, _ = fmt.Fprintln(w, rep.Message)
	}
	return rep.Exit
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

// argValue is the value after flag in args, or of flag=value; "" for none.
func argValue(args []string, flags ...string) string {
	for i, a := range args {
		for _, flag := range flags {
			if a == flag && i+1 < len(args) {
				return args[i+1]
			}
			if v, ok := strings.CutPrefix(a, flag+"="); ok {
				return v
			}
		}
	}
	return ""
}

// alive reports whether process pid still runs (a zombie does not).
func alive(pid int) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	if i := bytes.LastIndexByte(raw, ')'); i >= 0 && i+2 < len(raw) {
		return raw[i+2] != 'Z'
	}
	return true
}

// startTicks is this process's start time in clock ticks since boot, as
// the record of a CLI names its process.
func startTicks() string {
	raw, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return ""
	}
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 {
		return ""
	}
	f := strings.Fields(string(raw[i+1:]))
	if len(f) < 20 {
		return ""
	}
	return f[19]
}

// untilSignal blocks until the process is told to end.
func untilSignal() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	<-sig
}

// forever blocks without ever ending: a hung turn. The default signal
// handling stays, so a SIGTERM still kills the process.
func forever() {
	select {}
}

// toolVersion is the version of a host's pinned tool: the VERSION file of
// its installation (<installation>/bin/<tool>).
func toolVersion(exe string) string {
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(exe)), "VERSION"))
	if err != nil {
		return "0.0.0"
	}
	return strings.TrimSpace(string(raw))
}

// ghModeled says whether a host's pinned gh answers args itself: its
// version and whether it has a login.
func ghModeled(args []string) bool {
	return len(args) == 1 && args[0] == "--version" || len(args) == 2 && args[0] == "auth" && args[1] == "token"
}

func hostGh(c *client) int {
	if c.args[0] == "--version" {
		fmt.Printf("gh version %s (2026-01-01)\nhttps://github.com/cli/cli/releases/tag/v%s\n", toolVersion(c.exe), toolVersion(c.exe))
		return 0
	}
	// A host has no login of its own: the coordinator publishes for it.
	fmt.Fprintln(os.Stderr, "no oauth token found for github.com")
	return 1
}

// stepTimeout bounds one step of a worker's script.
const stepTimeout = 2 * time.Minute

// runSteps runs a turn's steps as the session's tool commands, in dir
// under env, and returns each step as it ran; failure is why the script
// stopped before its end. each, when set, sees every command step.
func runSteps(plan *turnPlan, dir string, env []string, each func(line string, r stepResult)) (results []stepResult, failure string) {
	vars := map[string]string{}
	for k, v := range plan.Vars {
		vars[k] = v
	}
	for i, st := range plan.Steps {
		stepDir := dir
		if st.Dir != "" {
			d := Expand(st.Dir, vars)
			if filepath.IsAbs(d) {
				stepDir = d
			} else {
				stepDir = filepath.Join(dir, d)
			}
		}
		if st.Write != nil {
			p := Expand(st.Write.Path, vars)
			if !filepath.IsAbs(p) {
				p = filepath.Join(stepDir, p)
			}
			r := stepResult{Line: "write " + p}
			err := os.MkdirAll(filepath.Dir(p), 0o750)
			if err == nil {
				err = os.WriteFile(p, []byte(Expand(st.Write.Content, vars)), 0o600)
			}
			if err != nil {
				r.Exit, r.Output = 1, err.Error()
				return append(results, r), fmt.Sprintf("step %d %s: %v", i, r.Line, err)
			}
			results = append(results, r)
			continue
		}
		argv := make([]string, 0, len(st.Argv()))
		for _, a := range st.Argv() {
			argv = append(argv, Expand(a, vars))
		}
		r := stepResult{Line: commandLine(argv)}
		bin, err := lookPath(argv[0], env)
		if err != nil {
			r.Exit, r.Output = 127, err.Error()
			return append(results, r), fmt.Sprintf("step %d `%s`: %v", i, r.Line, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
		cmd := exec.CommandContext(ctx, bin, argv[1:]...) //nolint:gosec // the worker script of the scenario
		cmd.Args[0] = argv[0]
		cmd.Dir, cmd.Env = stepDir, env
		cmd.Stdin = strings.NewReader(Expand(st.Stdin, vars))
		cmd.WaitDelay = 5 * time.Second
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		runErr := cmd.Run()
		timedOut := ctx.Err() != nil
		cancel()
		var ee *exec.ExitError
		switch {
		case runErr == nil:
		case errors.As(runErr, &ee):
			r.Exit = ee.ExitCode()
		default:
			r.Exit = -1
		}
		r.Output = clip(strings.TrimSpace(strings.TrimSpace(stdout.String())+"\n"+strings.TrimSpace(stderr.String())), 4000)
		if each != nil {
			each(r.Line, r)
		}
		results = append(results, r)
		if timedOut {
			return results, fmt.Sprintf("step %d `%s` still ran after %s", i, r.Line, stepTimeout)
		}
		if r.Exit != st.Exit {
			return results, fmt.Sprintf("step %d `%s` exited %d, want %d: %s", i, r.Line, r.Exit, st.Exit, clip(r.Output, 1500))
		}
		if st.Save != "" {
			vars[st.Save] = strings.TrimSpace(stdout.String())
		}
	}
	return results, ""
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("… (%d bytes)", len(s))
}

// commandLine is argv as a shell would read it.
func commandLine(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\n\"'$`\\#&|;<>(){}*?[]~") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		parts[i] = a
	}
	return strings.Join(parts, " ")
}

// finishTurn tells the world how a turn went.
func (c *client) finishTurn(s sessionID, plan *turnPlan, results []stepResult, failure string) {
	s.Prompt = ""
	_, _ = c.ask(request{Op: opTurnDone, Session: &s, Report: &report{Index: plan.Index, Steps: results, Outcome: plan.Outcome, Failure: failure}})
}

// beginTurn asks the world for a session's next turn; nil when the world
// does not answer, with the reason on standard error.
func (c *client) beginTurn(s sessionID) *turnPlan {
	rep, err := c.ask(request{Op: opTurn, Session: &s})
	if err != nil || rep.Turn == nil {
		fmt.Fprintf(os.Stderr, "%s: no turn from the scenario's world: %v\n", c.name, err)
		return nil
	}
	return rep.Turn
}
