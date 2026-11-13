//go:build scenario

package harness

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The harnesses the package fakes, by the name of their CLI.
const (
	Claude = "claude"
	Codex  = "codex"
	Agy    = "agy"
	Omp    = "omp"
)

// Harnesses are the fake coding agent CLIs.
var Harnesses = []string{Claude, Codex, Agy, Omp}

// Programs are the executables the package answers itself: the harnesses,
// the service manager and ssh. On an execution host, go is faked too.
var Programs = []string{Claude, Codex, Agy, Omp, "systemd-run", "systemctl", "ssh"}

// A turn's outcomes.
const (
	// Finish ends the turn as a turn that did its work ends: the reply is
	// written, the session stays as its harness leaves it between turns.
	Finish = "finish"
	// Hang never ends the turn: the CLI stays alive and silent.
	Hang = "hang"
	// Crash ends the CLI in the middle of the turn with a non-zero exit
	// and no reply.
	Crash = "crash"
	// Budget ends the turn on the provider's limit: the harness reports
	// it as the real one does and its usage probe says the window is full.
	Budget = "budget"
)

// Outcomes are a turn's outcomes.
var Outcomes = []string{Finish, Hang, Crash, Budget}

// Given is the harness block of a scenario: the worker scripts, what each
// harness's usage probe reports and the models the harnesses list.
type Given struct {
	// Workers are the worker scripts: a session runs the first whose name
	// and harness match. A worker without a script ends its turn idle.
	Workers []Worker `yaml:"workers"`
	// Usage is what a harness's usage probe reports, by harness.
	Usage map[string]Usage `yaml:"usage"`
	// Models are the models a harness lists (agy models, omp models), by
	// harness; DefaultModels when a harness names none.
	Models map[string][]string `yaml:"models"`
}

// DefaultModels are the models agy and omp list in a scenario that names
// none.
var DefaultModels = map[string][]string{
	Agy: {"gemini-3-pro", "gemini-3-flash"},
	Omp: {"local/qwen"},
}

// Worker is one worker script: what the sessions named Name do, turn by
// turn.
type Worker struct {
	// Name is a regular expression the session's name matches whole;
	// empty matches every session.
	Name string `yaml:"name"`
	// Harness limits the script to one harness; empty: every harness.
	Harness string `yaml:"harness"`
	// Host limits the script to the sessions of one execution host, by its
	// alias; empty: every machine, "local" the desk itself.
	Host string `yaml:"host"`
	// Turns are the session's turns in order. A turn beyond them does
	// nothing and finishes, unless Repeat repeats the last one.
	Turns  []Turn `yaml:"turns"`
	Repeat bool   `yaml:"repeat"`
}

// Turn is one turn of a worker: the steps it takes, then its outcome.
type Turn struct {
	Steps []Step `yaml:"steps"`
	// Outcome is finish (the default), hang, crash or budget.
	Outcome string `yaml:"outcome"`
	// Exit is a crash's exit code (1).
	Exit int `yaml:"exit"`
	// Tokens are the tokens the turn reports having used, in its
	// transcript or stream as the harness reports them.
	Tokens *Tokens `yaml:"tokens"`
	// Usage, when set, is what the harness's usage probe reports from the
	// end of this turn on. A budget turn fills the session window.
	Usage *Usage `yaml:"usage"`
	// Reply is the text of the turn's last message ("done").
	Reply string `yaml:"reply"`
}

// Tokens are the tokens of a turn.
type Tokens struct {
	Input  int `yaml:"input" json:"input"`
	Output int `yaml:"output" json:"output"`
}

// Usage is a provider's use of its windows in percent: Session the short
// window (Claude's 5 hours, Codex's primary), Week the long one.
type Usage struct {
	Session float64 `yaml:"session" json:"session"`
	Week    float64 `yaml:"week" json:"week"`
	// Fail makes the account refuse a stretch of the usage probe's
	// requests (codex only, in harness.usage).
	Fail *UsageFail `yaml:"fail" json:"-"`
}

// UsageFail is a stretch of the account's usage requests that fail: after
// the first After requests are answered, the next Times (every one when 0)
// get Message as codex app-server's error reply, with the JSON-RPC error
// Code (-32603, an internal error, when 0), or with Hang no answer at all:
// the app-server starts a helper that keeps its standard output open, as a
// helper of the real one can, and waits. The requests after them are
// answered again.
type UsageFail struct {
	After   int    `yaml:"after" json:"after"`
	Times   int    `yaml:"times" json:"times"`
	Message string `yaml:"message" json:"message"`
	Code    int    `yaml:"code" json:"code,omitempty"`
	Hang    bool   `yaml:"hang" json:"hang,omitempty"`
}

// fails reports whether the request after n answered or refused ones fails.
func (f *UsageFail) fails(n int) bool {
	return f != nil && n >= f.After && (f.Times == 0 || n < f.After+f.Times)
}

// Step is one step of a turn: exactly one of beekeeper, git, gh, run and
// write. The step's exit code must be Exit (0): a worker whose park or
// push fails fails the scenario, it is not a silent no-op.
type Step struct {
	// Beekeeper, Git and Gh run the real beekeeper, the real git and the
	// gh of the scenario with these arguments, as the session's tool
	// command: in its directory, under its identity.
	Beekeeper []string `yaml:"beekeeper" json:"beekeeper,omitempty"`
	Git       []string `yaml:"git" json:"git,omitempty"`
	Gh        []string `yaml:"gh" json:"gh,omitempty"`
	// Run runs any program on the session's PATH.
	Run []string `yaml:"run" json:"run,omitempty"`
	// Write writes a file, relative to the step's directory.
	Write *WriteFile `yaml:"write" json:"write,omitempty"`
	// Dir is the step's directory, relative to the session's (its own).
	Dir string `yaml:"dir" json:"dir,omitempty"`
	// Stdin is the command's standard input.
	Stdin string `yaml:"stdin" json:"stdin,omitempty"`
	// Exit is the exit code the step must end with.
	Exit int `yaml:"exit" json:"exit,omitempty"`
	// Save keeps the command's trimmed output under this name: a later
	// step's arguments name it as ${name}.
	Save string `yaml:"save" json:"save,omitempty"`
}

// WriteFile is a file a step writes.
type WriteFile struct {
	Path    string `yaml:"path" json:"path"`
	Content string `yaml:"content" json:"content"`
}

// Argv is the step's command line; nil for a write.
func (s Step) Argv() []string {
	switch {
	case s.Beekeeper != nil:
		return append([]string{"beekeeper"}, s.Beekeeper...)
	case s.Git != nil:
		return append([]string{"git"}, s.Git...)
	case s.Gh != nil:
		return append([]string{"gh"}, s.Gh...)
	case s.Run != nil:
		return s.Run
	}
	return nil
}

func (s Step) kinds() int {
	n := 0
	for _, set := range []bool{s.Beekeeper != nil, s.Git != nil, s.Gh != nil, s.Run != nil, s.Write != nil} {
		if set {
			n++
		}
	}
	return n
}

var (
	varRE   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	aliasRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	repoRE  = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// builtins are the names every step may expand: the session's own.
var builtins = []string{"name", "session", "harness", "dir", "turn", "host"}

// LocalHost names the desk itself where a script or a step names a host.
const LocalHost = "local"

// Validate checks the block: known harnesses and outcomes, patterns that
// compile, one kind per step and saved names defined before their use.
func (g *Given) Validate(hosts []Host) error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	for name := range g.Usage {
		if name != Claude && name != Codex {
			bad("harness.usage: %q has no usage probe the harness fakes (claude, codex)", name)
		}
	}
	for name, u := range g.Usage {
		if u.Session < 0 || u.Session > 100 || u.Week < 0 || u.Week > 100 {
			bad("harness.usage.%s: percent between 0 and 100", name)
		}
		if f := u.Fail; f != nil {
			switch {
			case name != Codex:
				bad("harness.usage.%s: fail: only codex's usage probe fails (its app-server's account request)", name)
			case f.Hang && f.Message != "":
				bad("harness.usage.%s: fail: a hung request has no message", name)
			case !f.Hang && strings.TrimSpace(f.Message) == "":
				bad("harness.usage.%s: fail: no message", name)
			case f.After < 0 || f.Times < 0:
				bad("harness.usage.%s: fail: after and times are counts", name)
			}
		}
	}
	for name, models := range g.Models {
		if name != Agy && name != Omp {
			bad("harness.models: %q lists no models the harness fakes (agy, omp)", name)
		}
		if len(models) == 0 {
			bad("harness.models.%s: no model", name)
		}
	}
	for i, w := range g.Workers {
		where := fmt.Sprintf("harness.workers[%d]", i)
		if _, err := regexp.Compile(w.Name); err != nil {
			bad("%s: name: %v", where, err)
		}
		if w.Harness != "" && !slices.Contains(Harnesses, w.Harness) {
			bad("%s: harness %q is none of %s", where, w.Harness, strings.Join(Harnesses, ", "))
		}
		if w.Host != "" && w.Host != LocalHost && !slices.ContainsFunc(hosts, func(h Host) bool { return h.Name == w.Host }) {
			bad("%s: host %q is neither local nor one of given.hosts", where, w.Host)
		}
		if len(w.Turns) == 0 {
			bad("%s: no turns: a worker without a script needs no entry", where)
		}
		for j, t := range w.Turns {
			at := fmt.Sprintf("%s.turns[%d]", where, j)
			if t.Outcome != "" && !slices.Contains(Outcomes, t.Outcome) {
				bad("%s: outcome %q is none of %s", at, t.Outcome, strings.Join(Outcomes, ", "))
			}
			if t.Exit != 0 && t.Outcome != Crash {
				bad("%s: exit is a crash's exit code", at)
			}
			if t.Exit < 0 || t.Exit > 255 {
				bad("%s: exit %d", at, t.Exit)
			}
			if u := t.Usage; u != nil && (u.Session < 0 || u.Session > 100 || u.Week < 0 || u.Week > 100) {
				bad("%s: usage: percent between 0 and 100", at)
			}
			if u := t.Usage; u != nil && u.Fail != nil {
				bad("%s: usage: fail is given in harness.usage, not in a turn", at)
			}
			saved := slices.Clone(builtins)
			for k, st := range t.Steps {
				sat := fmt.Sprintf("%s.steps[%d]", at, k)
				if st.kinds() != 1 {
					bad("%s: exactly one of beekeeper, git, gh, run and write", sat)
					continue
				}
				if st.Write != nil {
					if st.Write.Path == "" {
						bad("%s: write without a path", sat)
					}
					if st.Save != "" || st.Stdin != "" || st.Exit != 0 {
						bad("%s: save, stdin and exit are a command's", sat)
					}
				} else if len(st.Argv()) == 0 || st.Run != nil && len(st.Run) == 0 {
					bad("%s: a command without arguments", sat)
				}
				for _, text := range append(slices.Clone(st.Argv()), st.Dir, st.Stdin) {
					for _, name := range expansions(text) {
						if !slices.Contains(saved, name) {
							bad("%s: ${%s} is neither saved by an earlier step nor one of %s", sat, name, strings.Join(builtins, ", "))
						}
					}
				}
				if st.Save != "" {
					if !varRE.MatchString(st.Save) || slices.Contains(builtins, st.Save) {
						bad("%s: save %q: a lower-case name other than %s", sat, st.Save, strings.Join(builtins, ", "))
					}
					saved = append(saved, st.Save)
				}
			}
		}
	}
	return errors.Join(errs...)
}

var expansionRE = regexp.MustCompile(`\$\{([^}]*)\}`)

// expansions are the names text expands.
func expansions(text string) []string {
	var out []string
	for _, m := range expansionRE.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

// Expand replaces every ${name} in text with its value.
func Expand(text string, vars map[string]string) string {
	return expansionRE.ReplaceAllStringFunc(text, func(m string) string {
		return vars[m[2:len(m)-1]]
	})
}

// Duration is a Go duration string, always positive.
type Duration struct{ time.Duration }

// UnmarshalYAML parses a duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	if v <= 0 {
		return fmt.Errorf("line %d: duration %q must be positive", n.Line, s)
	}
	d.Duration = v
	return nil
}

// MarshalYAML writes the duration string.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// The faults of an execution host.
const (
	// Unreachable refuses the connection: ssh exits 255.
	Unreachable = "unreachable"
	// HostKeyChanged fails the host key check: ssh exits 255 with OpenSSH's
	// warning, as it does under StrictHostKeyChecking.
	HostKeyChanged = "host-key-changed"
	// MissingTool takes beekeeper off the host: the remote shell exits 127.
	MissingTool = "missing-tool"
	// OlderGo pins a Go on the host that is older than the go.mod of the
	// repository it serves names.
	OlderGo = "older-go"
)

// Faults are an execution host's faults.
var Faults = []string{Unreachable, HostKeyChanged, MissingTool, OlderGo}

// Host is an SSH execution host of the scenario: an alias the fake ssh maps
// to a root of its own under /work/hosts/<name>, with a home, a tool
// directory and a host configuration, provisioned as an operator would
// provision it and prepared with the real beekeeper.
type Host struct {
	// Name is the host's ssh alias, its agents.hosts selector and its
	// identity.
	Name string `yaml:"name"`
	// Harnesses are the harnesses the host offers (claude).
	Harnesses []string `yaml:"harnesses"`
	// Repositories are the repositories the host serves (every one of
	// given.repositories): each gets a source checkout on the host.
	Repositories []string `yaml:"repositories"`
	// MaxBusy is the host's maxBusy (2).
	MaxBusy int `yaml:"maxBusy"`
	// Fault is the fault the host starts with; Slow delays every ssh call
	// to it.
	Fault string   `yaml:"fault"`
	Slow  Duration `yaml:"slow"`
	// Config is laid over the host's own generated configuration.
	Config map[string]any `yaml:"config"`
}

// ValidateHosts checks the hosts of a scenario against its repositories.
func ValidateHosts(hosts []Host, repositories []string) error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	seen := map[string]bool{}
	for i, h := range hosts {
		where := fmt.Sprintf("hosts[%d]", i)
		if !aliasRE.MatchString(h.Name) || h.Name == LocalHost {
			bad("%s: name %q: lower-case letters, digits and dashes, and not local", where, h.Name)
		}
		if seen[h.Name] {
			bad("%s: a second host %q", where, h.Name)
		}
		seen[h.Name] = true
		for _, name := range h.Harnesses {
			if !slices.Contains(Harnesses, name) {
				bad("%s: harness %q is none of %s", where, name, strings.Join(Harnesses, ", "))
			}
		}
		if h.Fault != "" && !slices.Contains(Faults, h.Fault) {
			bad("%s: fault %q is none of %s", where, h.Fault, strings.Join(Faults, ", "))
		}
		if h.MaxBusy < 0 {
			bad("%s: maxBusy %d", where, h.MaxBusy)
		}
		for _, r := range h.Repositories {
			if !repoRE.MatchString(r) || !slices.Contains(repositories, r) {
				bad("%s: repository %q is none of given.repositories", where, r)
			}
		}
		if len(h.Repositories) == 0 && len(repositories) == 0 {
			bad("%s: a host serves a repository: given.repositories is empty", where)
		}
	}
	return errors.Join(errs...)
}

// HostStep is the host step of a scenario: it changes one host's faults
// while the scenario runs.
type HostStep struct {
	// Name is the host.
	Name string `yaml:"name"`
	// Fault arms a fault; Clear takes every fault and the delay off.
	Fault string   `yaml:"fault"`
	Slow  Duration `yaml:"slow"`
	Clear bool     `yaml:"clear"`
}

// Validate checks the step against the scenario's hosts.
func (h HostStep) Validate(hosts []Host) error {
	if !slices.ContainsFunc(hosts, func(x Host) bool { return x.Name == h.Name }) {
		return fmt.Errorf("host %q is none of given.hosts", h.Name)
	}
	if h.Fault != "" && !slices.Contains(Faults, h.Fault) {
		return fmt.Errorf("fault %q is none of %s", h.Fault, strings.Join(Faults, ", "))
	}
	if h.Fault == OlderGo {
		return errors.New("older-go is a host's provisioning: name it in given.hosts")
	}
	n := 0
	for _, set := range []bool{h.Fault != "", h.Slow.Duration > 0, h.Clear} {
		if set {
			n++
		}
	}
	if n == 0 || h.Clear && n > 1 {
		return errors.New("one of fault, slow and clear (clear alone)")
	}
	return nil
}

// WorkerExpect checks what the scripted worker sessions named Name did, as
// the world recorded it: Turns the number of turns they began, Outcome the
// outcome of the last one, Running whether a CLI of theirs still runs.
type WorkerExpect struct {
	// Name is a regular expression a session's name matches whole.
	Name    string `yaml:"name"`
	Harness string `yaml:"harness"`
	Host    string `yaml:"host"`
	Turns   *int   `yaml:"turns"`
	Outcome string `yaml:"outcome"`
	Running *bool  `yaml:"running"`
}

// Validate checks the expectation.
func (e WorkerExpect) Validate() error {
	if _, err := regexp.Compile(e.Name); err != nil {
		return fmt.Errorf("name: %w", err)
	}
	if e.Harness != "" && !slices.Contains(Harnesses, e.Harness) {
		return fmt.Errorf("harness %q is none of %s", e.Harness, strings.Join(Harnesses, ", "))
	}
	if e.Outcome != "" && !slices.Contains(Outcomes, e.Outcome) {
		return fmt.Errorf("outcome %q is none of %s", e.Outcome, strings.Join(Outcomes, ", "))
	}
	if e.Turns == nil && e.Outcome == "" && e.Running == nil {
		return errors.New("one of turns, outcome and running")
	}
	return nil
}
