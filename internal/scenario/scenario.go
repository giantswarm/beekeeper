//go:build scenario

package scenario

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/internal/scenario/fakegithub"
	"github.com/giantswarm/beekeeper/internal/scenario/harness"
)

// DefaultTimeout is a scenario's real-time bound when it names none.
const DefaultTimeout = 60 * time.Second

// Duration is a Go duration string in a scenario ("2s", "1m30s"), always
// positive.
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

// Scenario is one behavior stated as given, when and then.
type Scenario struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	// Issue is the issue the scenario covers, a bug's for a regression.
	Issue string   `yaml:"issue"`
	Flow  string   `yaml:"flow"`
	Tags  []string `yaml:"tags"`
	// Timeout bounds the scenario's real time (DefaultTimeout).
	Timeout Duration `yaml:"timeout"`
	// Pending names the open issue of a bug the scenario shows: the run
	// expects it to fail and fails once it passes.
	Pending string `yaml:"pending"`
	// Control opts the scenario out of the control (control.go) with the
	// reason it cannot fail without its pull request's change.
	Control *Control `yaml:"control"`
	Given   Given    `yaml:"given"`
	When    []Step   `yaml:"when"`
	Then    []Expect `yaml:"then"`

	// File is the file it was loaded from.
	File string `yaml:"-"`
}

// Given is the world a scenario starts in.
type Given struct {
	Desk Desk `yaml:"desk"`
	// Repositories each get a bare Git origin with one commit on main.
	Repositories []Repository `yaml:"repositories"`
	// Calls answer external programs; a call none answers fails the
	// scenario.
	Calls []Call `yaml:"calls"`
	// GitHub is the scenario's GitHub: a model behind REST and GraphQL
	// that answers gh and the API base variables (docs/scenarios-github.md).
	// Its repositories get origins like Repositories.
	GitHub *fakegithub.Given `yaml:"github"`
	// Harness scripts the workers the fake harnesses run and sets what
	// their usage probes report.
	Harness harness.Given `yaml:"harness"`
	// Hosts are the SSH execution hosts: each gets a root of its own and
	// an agents.hosts entry of the desk.
	Hosts []harness.Host `yaml:"hosts"`
	// Builder is the Kubernetes builder of the desk's run.remote: a fake
	// kubectl and one builder image.
	Builder *harness.Builder `yaml:"builder"`
	// State seeds the desk's state: a generated desk of some size.
	State *State `yaml:"state"`
	// Clock is the world's clock.
	Clock Clock `yaml:"clock"`
	// Tools are further programs on the desk's PATH (go, golangci-lint):
	// fakes like Fakes, which only given.calls answer.
	Tools []string `yaml:"tools"`
	// Releases are beekeeper's own releases, for self-update: the one the
	// desk runs and those the scenario's GitHub publishes.
	Releases Releases `yaml:"releases"`
}

// Desk is the desk's configuration: Config is laid over the generated
// config.yaml, map by map.
type Desk struct {
	Config map[string]any `yaml:"config"`
}

// Repository is a repository on the fake origin, owner/name.
type Repository struct {
	Name string `yaml:"name"`
}

// Call is one answer of the world: the first call of Program whose
// arguments match every pattern of Args (each a regular expression one
// argument matches whole) is answered with Respond, at most Times times
// (0: every time).
type Call struct {
	Program string   `yaml:"program"`
	Args    []string `yaml:"args"`
	Respond Response `yaml:"respond"`
	Times   int      `yaml:"times"`
}

// Response is what a fake prints and exits with.
type Response struct {
	Stdout string `yaml:"stdout"`
	Stderr string `yaml:"stderr"`
	Exit   int    `yaml:"exit"`
}

// Step is one when step: exactly one of run, background, stop, wait_for,
// advance, github, fault and host, with optional expectations of its own
// response.
type Step struct {
	Name string `yaml:"name"`
	// Run runs the candidate to its exit.
	Run *Run `yaml:"run"`
	// Background starts the candidate and leaves it running until a stop
	// step or the scenario's end, which stop it by PID.
	Background *Run `yaml:"background"`
	// Stop stops the background step of that name.
	Stop string `yaml:"stop"`
	// WaitFor polls its expectations until they hold, bounded.
	WaitFor *WaitFor `yaml:"wait_for"`
	// Advance moves the world's clock forward.
	Advance *Advance `yaml:"advance"`
	// GitHub is something that happens on the scenario's GitHub; Fault
	// arms or clears one of its faults.
	GitHub *fakegithub.Step      `yaml:"github"`
	Fault  *fakegithub.FaultStep `yaml:"fault"`
	// Host changes an execution host's faults.
	Host   *harness.HostStep `yaml:"host"`
	Expect []Expect          `yaml:"expect"`
}

// Run is a call of the candidate binary, or with Release of that release's
// build (given.releases): a process still on a release the desk moved from.
type Run struct {
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
	Stdin   string            `yaml:"stdin"`
	Release string            `yaml:"release"`
}

// WaitFor polls Expect every Interval (100ms) until all hold or Timeout,
// which is required.
type WaitFor struct {
	Expect   []Expect `yaml:"expect"`
	Timeout  Duration `yaml:"timeout"`
	Interval Duration `yaml:"interval"`
}

// The step kinds.
const (
	KindRun        = "run"
	KindBackground = "background"
	KindStop       = "stop"
	KindWaitFor    = "wait_for"
	KindAdvance    = "advance"
	KindGitHub     = "github"
	KindFault      = "fault"
	KindHost       = "host"
)

// Kind is the step's kind, "" unless exactly one is set.
func (s Step) Kind() string {
	var kinds []string
	if s.Run != nil {
		kinds = append(kinds, KindRun)
	}
	if s.Background != nil {
		kinds = append(kinds, KindBackground)
	}
	if s.Stop != "" {
		kinds = append(kinds, KindStop)
	}
	if s.WaitFor != nil {
		kinds = append(kinds, KindWaitFor)
	}
	if s.Advance != nil {
		kinds = append(kinds, KindAdvance)
	}
	if s.GitHub != nil {
		kinds = append(kinds, KindGitHub)
	}
	if s.Fault != nil {
		kinds = append(kinds, KindFault)
	}
	if s.Host != nil {
		kinds = append(kinds, KindHost)
	}
	if len(kinds) != 1 {
		return ""
	}
	return kinds[0]
}

// Expect is one expectation: exactly one kind, of the step named by Step
// (in then; the last step by default) or of the step it is inline in.
type Expect struct {
	Step     string          `yaml:"step"`
	Exit     *int            `yaml:"exit"`
	Output   *TextExpect     `yaml:"output"`
	Stderr   *TextExpect     `yaml:"stderr"`
	JSONPath *JSONPathExpect `yaml:"json_path"`
	Events   *EventsExpect   `yaml:"events"`
	Duration *DurationExpect `yaml:"duration"`
	Process  *ProcessExpect  `yaml:"process"`
	Calls    *CallsExpect    `yaml:"calls"`
	// GitHub checks the scenario's GitHub: its calls, its budgets, a pull
	// request or an issue.
	GitHub *fakegithub.Expect `yaml:"github"`
	// Worker checks what a scripted worker did, Origin a branch of a
	// repository's origin.
	Worker *harness.WorkerExpect `yaml:"worker"`
	Origin *OriginExpect         `yaml:"origin"`
	// Log checks what a background step has written so far.
	Log *LogExpect `yaml:"log"`
}

// TextExpect checks a text: every Contains is in it, no NotContains is,
// and Matches (a regular expression, multi-line) matches it.
type TextExpect struct {
	Contains    []string `yaml:"contains"`
	NotContains []string `yaml:"not_contains"`
	Matches     string   `yaml:"matches"`
}

// JSONPathExpect checks the value at Path of the step's output read as
// JSON: a dotted path with list indexes ("pick.ref", "items[0].ref", "."
// for the whole document). Equals compares the value; Exists whether the
// path resolves; Len the length of a list, map or string.
type JSONPathExpect struct {
	Path   string `yaml:"path"`
	Equals any    `yaml:"equals"`
	Exists *bool  `yaml:"exists"`
	Len    *int   `yaml:"len"`
}

// EventsExpect checks the verbs of the desk's event log.
type EventsExpect struct {
	Contains    []string `yaml:"contains"`
	NotContains []string `yaml:"not_contains"`
}

// DurationExpect bounds a step's real time.
type DurationExpect struct {
	Max Duration `yaml:"max"`
}

// ProcessExpect checks the background step Name: Running whether it still
// runs, Exit its exit code once it exited.
type ProcessExpect struct {
	Name    string `yaml:"name"`
	Running *bool  `yaml:"running"`
	Exit    *int   `yaml:"exit"`
}

// CallsExpect counts the world's calls so far of Program (every program
// when empty) whose arguments match every pattern of Args.
type CallsExpect struct {
	Program string   `yaml:"program"`
	Args    []string `yaml:"args"`
	Min     *int     `yaml:"min"`
	Max     *int     `yaml:"max"`
}

// OriginExpect checks a branch of Repository's bare origin: Branch is a
// regular expression a branch's name matches whole, Exists whether such a
// branch is there (true by default), Ahead how many commits it is ahead
// of the origin's main.
type OriginExpect struct {
	Repository string `yaml:"repository"`
	Branch     string `yaml:"branch"`
	Exists     *bool  `yaml:"exists"`
	Ahead      *int   `yaml:"ahead"`
}

// kinds names the expectation's kinds that are set.
func (e Expect) kinds() []string {
	var k []string
	for name, set := range map[string]bool{
		"exit": e.Exit != nil, "output": e.Output != nil, "stderr": e.Stderr != nil,
		"json_path": e.JSONPath != nil, "events": e.Events != nil, "duration": e.Duration != nil,
		"process": e.Process != nil, "calls": e.Calls != nil, "github": e.GitHub != nil,
		"worker": e.Worker != nil, "origin": e.Origin != nil, "log": e.Log != nil,
	} {
		if set {
			k = append(k, name)
		}
	}
	slices.Sort(k)
	return k
}

// Kind is the expectation's kind; "" unless exactly one is set.
func (e Expect) Kind() string {
	if k := e.kinds(); len(k) == 1 {
		return k[0]
	}
	return ""
}

// Fakes are the external programs the world answers, one fake executable
// dispatching on its name.
var Fakes = []string{"gh", "claude", "codex", "agy", "omp", "ssh", "devctl", "systemctl", "systemd-run", "journalctl"}

// ToolsEnv names a scenario's given.tools to the fake executable, which
// serves them beside Fakes.
const ToolsEnv = "BEEKEEPER_SCENARIO_TOOLS"

// reservedPrograms are the names on the desk's PATH no tool may take: the
// fake kubectl is given.builder's.
var reservedPrograms = []string{"beekeeper", "git", "sh", "real-git", "real-git-core", "git-core", "scenario-fake", harness.Kubectl}

// programs are the programs the scenario's world answers: Fakes and
// given.tools.
func (s *Scenario) programs() []string {
	return append(slices.Clone(Fakes), s.Given.Tools...)
}

var (
	toolRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	nameRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	issueRE = regexp.MustCompile(`^https://github\.com/[^/]+/[^/]+/issues/[0-9]+$`)
	repoRE  = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// Load reads and validates one scenario file.
func Load(path string) (*Scenario, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // a scenario file of the suite
	if err != nil {
		return nil, err
	}
	return Parse(path, raw)
}

// Parse decodes a scenario strictly: unknown keys, steps or expectations
// are errors, and so are a scenario without then and an unbounded wait_for.
func Parse(file string, raw []byte) (*Scenario, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var s Scenario
	if err := dec.Decode(&s); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: empty scenario", file)
		}
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	var more yaml.Node
	if err := dec.Decode(&more); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: one scenario per file", file)
	}
	s.File = file
	s.Given.publishReleases()
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return &s, nil
}

// repositoryNames are the scenario's repositories: given.repositories and
// given.github's, each once. Every one has an origin.
func (s *Scenario) repositoryNames() []string {
	var out []string
	for _, r := range s.Given.Repositories {
		out = append(out, r.Name)
	}
	if g := s.Given.GitHub; g != nil {
		for _, name := range g.RepositoryNames() {
			if !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	return out
}

// Validate checks what the decoder cannot: required fields, exactly one
// kind per step and expectation, references and patterns.
func (s *Scenario) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !nameRE.MatchString(s.Name) {
		bad("name %q: lower-case letters, digits and dashes", s.Name)
	}
	if !nameRE.MatchString(s.Flow) {
		bad("flow %q: lower-case letters, digits and dashes", s.Flow)
	}
	if s.Issue != "" && !issueRE.MatchString(s.Issue) {
		bad("issue %q: a full issue URL", s.Issue)
	}
	if err := s.Control.Validate(); err != nil {
		bad("%v", err)
	}
	if s.Pending != "" && !issueRE.MatchString(s.Pending) {
		bad("pending %q: the full URL of the open issue", s.Pending)
	}
	if s.Timeout.Duration == 0 {
		s.Timeout.Duration = DefaultTimeout
	}
	if _, err := s.Given.Clock.StartTime(); err != nil {
		bad("given.clock.start: %v", err)
	}
	for _, r := range s.Given.Repositories {
		if !repoRE.MatchString(r.Name) {
			bad("given.repositories: %q is not owner/name", r.Name)
		}
	}
	if g := s.Given.GitHub; g != nil {
		if err := g.Validate(); err != nil {
			bad("given.github: %v", err)
		}
	}
	for i, tool := range s.Given.Tools {
		switch {
		case !toolRE.MatchString(tool):
			bad("given.tools[%d]: %q is no program name", i, tool)
		case slices.Contains(Fakes, tool) || slices.Contains(reservedPrograms, tool):
			bad("given.tools[%d]: %q is on the desk's PATH already", i, tool)
		case slices.Index(s.Given.Tools, tool) != i:
			bad("given.tools[%d]: %q is named twice", i, tool)
		}
	}
	for _, line := range s.Given.Releases.validate() {
		bad("given.releases: %s", line)
	}
	for i, c := range s.Given.Calls {
		if !slices.Contains(s.programs(), c.Program) {
			bad("given.calls[%d]: program %q is none of %s", i, c.Program, strings.Join(s.programs(), ", "))
		}
		if c.Times < 0 {
			bad("given.calls[%d]: times %d", i, c.Times)
		}
		if _, err := compileArgs(c.Args); err != nil {
			bad("given.calls[%d]: %v", i, err)
		}
	}
	repositories := s.repositoryNames()
	for _, err := range []error{harness.ValidateHosts(s.Given.Hosts, repositories), s.Given.Harness.Validate(s.Given.Hosts), s.Given.Builder.Validate(), s.Given.State.validate(repositories)} {
		if err == nil {
			continue
		}
		for _, line := range strings.Split(err.Error(), "\n") {
			bad("given.%s", line)
		}
	}
	if len(s.When) == 0 {
		bad("no when: a scenario does something")
	}
	if len(s.Then) == 0 {
		bad("no then: a scenario without assertions passes vacuously")
	}
	names := map[string]string{}
	var background []string
	for i, st := range s.When {
		where := fmt.Sprintf("when[%d]", i)
		if st.Name != "" {
			where = fmt.Sprintf("when[%d] %s", i, st.Name)
			if !nameRE.MatchString(st.Name) {
				bad("%s: name: lower-case letters, digits and dashes", where)
			}
			if _, dup := names[st.Name]; dup {
				bad("%s: a second step of that name", where)
			}
		}
		kind := st.Kind()
		switch kind {
		case "":
			bad("%s: exactly one of run, background, stop, wait_for, advance, github, fault and host", where)
		case KindGitHub, KindFault:
			var err error
			if kind == KindGitHub {
				err = st.GitHub.Validate()
			} else {
				err = st.Fault.Validate()
			}
			if err != nil {
				bad("%s: %s: %v", where, kind, err)
			}
			if s.Given.GitHub == nil {
				bad("%s: a %s step needs given.github", where, kind)
			}
		case KindRun, KindBackground:
			r := st.Run
			if kind == KindBackground {
				r = st.Background
				if st.Name == "" {
					bad("%s: a background step needs a name to stop it by", where)
				}
				background = append(background, st.Name)
			}
			if len(r.Args) == 0 {
				bad("%s: %s without args", where, kind)
			}
			if r.Release != "" && !s.Given.Releases.known(r.Release) {
				bad("%s: release %q is none of given.releases", where, r.Release)
			}
		case KindStop:
			if !slices.Contains(background, st.Stop) {
				bad("%s: stop %q names no earlier background step", where, st.Stop)
			}
		case KindHost:
			if err := st.Host.Validate(s.Given.Hosts); err != nil {
				bad("%s: host: %v", where, err)
			}
		case KindWaitFor:
			if st.WaitFor.Timeout.Duration == 0 {
				bad("%s: wait_for without a timeout is unbounded", where)
			} else if st.WaitFor.Timeout.Duration > s.Timeout.Duration {
				bad("%s: wait_for timeout %s exceeds the scenario's %s", where, st.WaitFor.Timeout, s.Timeout)
			}
			if len(st.WaitFor.Expect) == 0 {
				bad("%s: wait_for without expectations", where)
			}
			for j, e := range st.WaitFor.Expect {
				errs = append(errs, s.validateExpect(fmt.Sprintf("%s wait_for[%d]", where, j), e, kind, background, false)...)
			}
		case KindAdvance:
			errs = append(errs, s.validateAdvance(where, st.Advance, background)...)
		}
		if st.Name != "" {
			names[st.Name] = kind
		}
		for j, e := range st.Expect {
			errs = append(errs, s.validateExpect(fmt.Sprintf("%s expect[%d]", where, j), e, kind, background, false)...)
		}
	}
	for i, e := range s.Then {
		kind := ""
		if len(s.When) > 0 {
			kind = s.When[len(s.When)-1].Kind()
		}
		if e.Step != "" {
			k, ok := names[e.Step]
			if !ok {
				bad("then[%d]: step %q names no step", i, e.Step)
				continue
			}
			kind = k
		}
		errs = append(errs, s.validateExpect(fmt.Sprintf("then[%d]", i), e, kind, background, true)...)
	}
	return errors.Join(errs...)
}

// stepObserves are the expectation kinds a step kind's response holds;
// events, calls and process are the desk's and the world's, held by all.
var stepObserves = map[string][]string{
	KindRun:        {"exit", "output", "stderr", "json_path", "duration"},
	KindBackground: {"duration"},
	KindStop:       {"duration"},
	KindWaitFor:    {"duration"},
	KindAdvance:    {"duration"},
	KindGitHub:     {"duration"},
	KindFault:      {"duration"},
	KindHost:       {"duration"},
}

func (s *Scenario) validateExpect(where string, e Expect, stepKind string, background []string, inThen bool) []error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(where+": "+format, args...)) }
	if e.Step != "" && !inThen {
		bad("step is for then; an inline expectation reads its own step")
	}
	kind := e.Kind()
	if kind == "" {
		bad("exactly one of exit, output, stderr, json_path, events, duration, process, calls, github, worker, origin and log (has %v)", e.kinds())
		return errs
	}
	switch kind {
	case "events", "calls", "process", "github", "worker", "origin", "log":
	default:
		if stepKind != "" && !slices.Contains(stepObserves[stepKind], kind) {
			bad("%s is not observed for a %s step", kind, stepKind)
		}
	}
	for _, t := range []*TextExpect{e.Output, e.Stderr} {
		if t == nil {
			continue
		}
		if len(t.Contains)+len(t.NotContains) == 0 && t.Matches == "" {
			bad("%s without contains, not_contains or matches", kind)
		}
		if t.Matches != "" {
			if _, err := regexp.Compile(t.Matches); err != nil {
				bad("matches: %v", err)
			}
		}
	}
	if j := e.JSONPath; j != nil {
		if _, err := parsePath(j.Path); err != nil {
			bad("json_path: %v", err)
		}
		n := 0
		if j.Equals != nil {
			n++
		}
		if j.Exists != nil {
			n++
		}
		if j.Len != nil {
			n++
		}
		if n != 1 {
			bad("json_path: exactly one of equals, exists and len")
		}
	}
	if ev := e.Events; ev != nil && len(ev.Contains)+len(ev.NotContains) == 0 {
		bad("events without contains or not_contains")
	}
	if d := e.Duration; d != nil && d.Max.Duration == 0 {
		bad("duration without max")
	}
	if p := e.Process; p != nil {
		if !slices.Contains(background, p.Name) {
			bad("process %q names no earlier background step", p.Name)
		}
		if (p.Running == nil) == (p.Exit == nil) {
			bad("process: exactly one of running and exit")
		}
	}
	if g := e.GitHub; g != nil {
		if err := g.Validate(); err != nil {
			bad("github: %v", err)
		}
		if s.Given.GitHub == nil {
			bad("a github expectation needs given.github")
		}
	}
	if w := e.Worker; w != nil {
		if err := w.Validate(); err != nil {
			bad("worker: %v", err)
		}
	}
	if o := e.Origin; o != nil {
		if !slices.Contains(s.repositoryNames(), o.Repository) {
			bad("origin: repository %q is none of given.repositories", o.Repository)
		}
		if o.Branch == "" {
			bad("origin without a branch")
		} else if _, err := regexp.Compile(o.Branch); err != nil {
			bad("origin: branch: %v", err)
		}
		if o.Exists != nil && !*o.Exists && o.Ahead != nil {
			bad("origin: ahead of a branch that is not there")
		}
	}
	if l := e.Log; l != nil {
		for _, line := range l.validate(background) {
			bad("%s", line)
		}
	}
	if c := e.Calls; c != nil {
		if c.Program != "" && !slices.Contains(s.called(), c.Program) {
			bad("calls: program %q is none of %s", c.Program, strings.Join(s.called(), ", "))
		}
		if _, err := compileArgs(c.Args); err != nil {
			bad("calls: %v", err)
		}
		if c.Min == nil && c.Max == nil {
			bad("calls without min or max")
		}
	}
	return errs
}

// called are the programs whose calls the world logs: the fakes,
// given.tools, and with given.builder the fake kubectl and the builder
// image's tools, each once.
func (s *Scenario) called() []string {
	out := s.programs()
	for _, p := range append([]string{harness.Kubectl}, harness.BuilderTools...) {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// compileArgs compiles argument patterns, each matching one argument whole
// ("." matches newlines too).
func compileArgs(args []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(args))
	for _, a := range args {
		re, err := regexp.Compile(`(?s)^(?:` + a + `)$`)
		if err != nil {
			return nil, fmt.Errorf("args %q: %w", a, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// LoadDir loads every scenario under root (root/<flow>/<name>.yaml): a
// file's flow must be its directory's name and every name unique.
func LoadDir(root string) ([]*Scenario, error) {
	var out []*Scenario
	var errs []error
	seen := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
			errs = append(errs, fmt.Errorf("%s: not a .yaml scenario", path))
			return nil
		}
		s, err := Load(path)
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		if dir := filepath.Base(filepath.Dir(path)); dir != s.Flow {
			errs = append(errs, fmt.Errorf("%s: flow %q lives in scenarios/%s/", path, s.Flow, s.Flow))
		}
		if other, dup := seen[s.Name]; dup {
			errs = append(errs, fmt.Errorf("%s: name %q is also %s's", path, s.Name, other))
		}
		seen[s.Name] = path
		out = append(out, s)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	// Start the longest-timeout scenarios first so a long regression run does
	// not trail alone at the end of a parallel suite.
	slices.SortFunc(out, func(a, b *Scenario) int {
		if c := cmp.Compare(b.Timeout.Duration, a.Timeout.Duration); c != 0 {
			return c
		}
		return strings.Compare(a.Flow+"/"+a.Name, b.Flow+"/"+b.Name)
	})
	return out, nil
}

// Filter keeps the scenarios of any of flows (all when empty) that carry
// any of tags (all when empty).
func Filter(all []*Scenario, flows, tags []string) []*Scenario {
	var out []*Scenario
	for _, s := range all {
		if len(flows) > 0 && !slices.Contains(flows, s.Flow) {
			continue
		}
		if len(tags) > 0 && !slices.ContainsFunc(tags, func(t string) bool { return slices.Contains(s.Tags, t) }) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// Shard is one of Total disjoint parts of the suite: CI runs each in its
// own container, so the suite grows without crossing its wall-time budget.
// The zero Shard is the whole suite.
type Shard struct {
	Index, Total int
}

// ParseShard reads "<index>/<total>" (index from 0); empty is the whole
// suite.
func ParseShard(s string) (Shard, error) {
	if strings.TrimSpace(s) == "" {
		return Shard{}, nil
	}
	i, n, ok := strings.Cut(strings.TrimSpace(s), "/")
	index, err1 := strconv.Atoi(i)
	total, err2 := strconv.Atoi(n)
	if !ok || err1 != nil || err2 != nil || total < 1 || index < 0 || index >= total {
		return Shard{}, fmt.Errorf("shard %q is not <index>/<total> with 0 <= index < total", s)
	}
	return Shard{Index: index, Total: total}, nil
}

// Holds reports whether s runs in this shard. A scenario's shard is the
// FNV-1a hash of its flow/name modulo Total: every scenario runs in exactly
// one shard, the suite and the control agree on it, and adding or removing
// a scenario moves no other.
func (sh Shard) Holds(s *Scenario) bool {
	if sh.Total <= 1 {
		return true
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(s.Flow + "/" + s.Name))
	return int(h.Sum32()%uint32(sh.Total)) == sh.Index //nolint:gosec // Total is a small positive count
}

// Of keeps the scenarios of this shard.
func (sh Shard) Of(all []*Scenario) []*Scenario {
	var out []*Scenario
	for _, s := range all {
		if sh.Holds(s) {
			out = append(out, s)
		}
	}
	return out
}

// String is the shard as ParseShard reads it, "all" for the whole suite.
func (sh Shard) String() string {
	if sh.Total <= 1 {
		return "all"
	}
	return fmt.Sprintf("%d/%d", sh.Index, sh.Total)
}

// SplitList splits a comma-separated list, dropping empty entries.
func SplitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
