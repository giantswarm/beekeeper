//go:build scenario && linux

package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/scenario/fakegithub"
	"github.com/giantswarm/beekeeper/internal/scenario/harness"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The desk inside a scenario's sandbox: everything lives under /work, the
// only writable mount.
const (
	workDir      = "/work"
	binDir       = workDir + "/bin"
	homeDir      = workDir + "/home"
	repoDir      = workDir + "/repo"
	stateDir     = workDir + "/state"
	originsDir   = workDir + "/origins"
	artifactsDir = workDir + "/artifacts"
	configFile   = workDir + "/config.yaml"
	worldSocket  = DefaultWorldSocket
	deskIdentity = "scenario"
	scenarioFile = workDir + "/scenario.yaml"
	resultFile   = workDir + "/result.json"
	candidate    = binDir + "/beekeeper"
	fakeBinary   = "scenario-fake"
)

// env is the allowlisted environment of every process the scenario starts:
// nothing of the host's, PATH only the fakes, the candidate and real git.
func env(extra map[string]string) []string {
	vars := map[string]string{
		"PATH":                "/work/bin",
		"HOME":                homeDir,
		"TMPDIR":              "/tmp",
		"LANG":                "C.UTF-8",
		"XDG_CONFIG_HOME":     homeDir + "/.config",
		"XDG_STATE_HOME":      homeDir + "/.local/state",
		"XDG_CACHE_HOME":      homeDir + "/.cache",
		"BEEKEEPER_CONFIG":    configFile,
		"BEEKEEPER_LOADAVG":   "0.10 0.10 0.10",
		WorldEnv:              worldSocket,
		clock.Env:             clockSocket,
		harness.SocketEnv:     harness.DefaultSocket,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   "/dev/null",
		"GIT_ALLOW_PROTOCOL":  "file:https",
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_AUTHOR_NAME":     "Scenario",
		"GIT_AUTHOR_EMAIL":    "scenario@example.invalid",
		"GIT_COMMITTER_NAME":  "Scenario",
		"GIT_COMMITTER_EMAIL": "scenario@example.invalid",
		"GOPROXY":             "off",
		"GOSUMDB":             "off",
	}
	maps.Copy(vars, extra)
	out := make([]string, 0, len(vars))
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		out = append(out, k+"="+vars[k])
	}
	return out
}

// proc is a background step's process, stopped by PID.
type proc struct {
	cmd     *exec.Cmd
	started time.Time
	done    chan struct{}
	mu      sync.Mutex
	exited  bool
	exit    int
}

func (p *proc) state() (running bool, exit int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.exited, p.exit
}

// stop ends the process group of p by PID: SIGTERM, then SIGKILL.
func (p *proc) stop() {
	if running, _ := p.state(); !running {
		return
	}
	pid := p.cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-p.done
	}
}

// driver runs one scenario's steps inside its sandbox.
type driver struct {
	s     *Scenario
	world *World
	clock *clock.Server
	// fakes is the world of the fake harnesses, units and hosts.
	fakes *harness.World
	res   *Result
	procs map[string]*proc
	views map[string]View
	last  View
	// github is the scenario's GitHub, nil without given.github; its
	// board is the desk's.
	github      *fakegithub.Server
	githubLog   *os.File
	boardOwner  string
	boardNumber int
	// cfg is the desk's configuration as setup wrote it, orgs its
	// github.allowedOrgs: the hosts are provisioned against them.
	cfg  map[string]any
	orgs []string
}

// deskCaller names the desk itself to the scenario's GitHub: what a unit
// of its service manager or a host's coordinator spends.
const deskCaller = "desk"

// Drive runs the scenario at /work/scenario.yaml inside the sandbox and
// writes its result to /work/result.json.
func Drive() error {
	started := time.Now()
	res := &Result{}
	s, err := Load(scenarioFile)
	if err != nil {
		res.Harness = err.Error()
	} else {
		res.Name, res.Flow, res.Pending = s.Name, s.Flow, s.Pending
		d := &driver{s: s, res: res, procs: map[string]*proc{}, views: map[string]View{}}
		d.run()
	}
	res.Duration = time.Since(started)
	raw, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(resultFile, raw, 0o600)
}

func (d *driver) run() {
	ctx, cancel := context.WithTimeout(context.Background(), d.s.Timeout.Duration)
	defer cancel()
	callLog, err := os.Create(filepath.Join(artifactsDir, "calls.jsonl"))
	if err != nil {
		d.res.Harness = err.Error()
		return
	}
	defer func() { _ = callLog.Close() }()
	var logMu sync.Mutex
	d.world, err = NewWorld(d.s.Given.Calls, writerFunc(func(b []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return callLog.Write(b)
	}))
	if err == nil {
		err = d.world.Serve(worldSocket)
	}
	if err == nil {
		err = d.startClock()
	}
	if err == nil {
		err = d.setup(ctx)
	}
	if err == nil {
		err = d.seedState()
	}
	if err == nil {
		err = d.tagReleases(ctx)
	}
	if err == nil {
		err = d.startGitHub(&logMu)
	}
	if err == nil {
		err = d.startFakes()
	}
	if err == nil {
		err = d.provisionHosts(ctx)
	}
	if err != nil {
		if d.fakes != nil {
			_ = d.fakes.Close()
		}
		d.res.Harness = err.Error()
		return
	}
	defer func() {
		for _, name := range slices.Sorted(maps.Keys(d.procs)) {
			d.procs[name].stop()
		}
		_ = d.fakes.Close()
		for _, f := range d.fakes.Failures() {
			d.res.Fail("%s", f)
		}
		_ = d.world.Close()
		if d.clock != nil {
			_ = d.clock.Close()
		}
		d.res.Calls = d.world.Calls()
		for _, c := range d.world.Unanswered() {
			if c.Reason != "" {
				d.res.Fail("unanswered call #%d: %s (%s)", c.Seq, c.Line(), c.Reason)
				continue
			}
			d.res.Fail("unanswered call #%d: %s", c.Seq, c.Line())
		}
		if d.github != nil {
			_ = d.github.Close()
			// A request for what the model lacks is never a guess: it
			// fails the scenario, like a call nothing answers.
			for _, c := range d.github.Unsupported() {
				d.res.Fail("the scenario's GitHub does not model %s (call #%d: %s)", c.Unsupported, c.Seq, c.Line())
			}
			_ = d.githubLog.Close()
		}
	}()
	for i, st := range d.s.When {
		if err := ctx.Err(); err != nil {
			d.res.Fail("scenario timed out after %s before when[%d]", d.s.Timeout, i)
			return
		}
		sr := d.step(ctx, i, st)
		d.res.Steps = append(d.res.Steps, sr)
		for _, f := range sr.Failures {
			d.res.Fail("when[%d] %s: %s", i, cmpOr(st.Name, sr.Kind), f)
		}
	}
	then := StepResult{Index: len(d.s.When), Kind: "then"}
	started := time.Now()
	for i, e := range d.s.Then {
		v := d.last
		if e.Step != "" {
			v = d.views[e.Step]
		}
		if err := Check(e, v); err != nil {
			then.Failures = append(then.Failures, fmt.Sprintf("then[%d] %s: %v", i, e.Kind(), err))
		}
	}
	then.Duration = time.Since(started)
	d.res.Steps = append(d.res.Steps, then)
	d.res.Failures = append(d.res.Failures, then.Failures...)
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// setup writes the desk: config.yaml from the scenario's desk over the
// base, the worker brief, the home directories and a bare origin per
// repository with one commit on main.
func (d *driver) setup(ctx context.Context) error {
	for _, dir := range []string{homeDir + "/.config", homeDir + "/.local/state", homeDir + "/.cache", stateDir, workDir + "/leases", originsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	orgs := []string{"example"}
	repos := d.repositories()
	for _, name := range repos {
		owner, _, _ := strings.Cut(name, "/")
		if !slices.Contains(orgs, owner) {
			orgs = append(orgs, owner)
		}
	}
	cfg := baseConfig(orgs)
	if b := d.s.Given.Builder; b != nil {
		// The desk's remote builds go to the scenario's builder, through
		// the fake kubectl on its PATH.
		remote, err := harness.ProvisionBuilder(b, binDir+"/"+fakeBinary)
		if err != nil {
			return fmt.Errorf("given.builder: %w", err)
		}
		if err := os.Symlink(fakeBinary, filepath.Join(binDir, harness.Kubectl)); err != nil {
			return err
		}
		overlay(cfg, map[string]any{"run": map[string]any{"remote": remote}})
	}
	overlay(cfg, d.s.Given.Desk.Config)
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if len(d.s.Given.Hosts) > 0 {
		// The hosts are provisioned against this first configuration, and
		// their agents.hosts entries come with that: until then it names
		// none, so supervisor.hosts of the scenario's desk waits as well.
		var first map[string]any
		if err := yaml.Unmarshal(raw, &first); err != nil {
			return err
		}
		if sup, ok := first["supervisor"].(map[string]any); ok {
			delete(sup, "hosts")
		}
		if raw, err = yaml.Marshal(first); err != nil {
			return err
		}
	}
	if err := os.WriteFile(configFile, raw, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(workDir, "worker.txt"), []byte("Work {{.URL}}\n"), 0o600); err != nil {
		return err
	}
	for _, name := range repos {
		if err := origin(ctx, name); err != nil {
			return fmt.Errorf("origin %s: %w", name, err)
		}
	}
	if board, ok := cfg["board"].(map[string]any); ok {
		d.boardOwner, _ = board["owner"].(string)
		d.boardNumber, _ = board["project"].(int)
	}
	d.cfg, d.orgs = cfg, orgs
	return codexCatalog(cfg, d.s.Given.Harness.Models[harness.Codex])
}

// codexCatalog writes the model catalog the codex CLI keeps beside its
// configuration. Beekeeper validates the effort of a codex start against
// it, and the loop's starts carry supervisor.effort. It lists the models of
// the desk's codex choices and of given.harness.models, each with every
// effort.
func codexCatalog(cfg map[string]any, models []string) error {
	slugs := slices.Clone(models)
	if capacity, ok := cfg["capacity"].(map[string]any); ok {
		choices, _ := capacity["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			if model, _ := choice["model"].(string); choice["harness"] == harness.Codex && model != "" && !slices.Contains(slugs, model) {
				slugs = append(slugs, model)
			}
		}
	}
	type level struct {
		Effort string `json:"effort"`
	}
	type model struct {
		Slug   string  `json:"slug"`
		Levels []level `json:"supported_reasoning_levels"`
	}
	catalog := struct {
		Models []model `json:"models"`
	}{Models: []model{}}
	for _, slug := range slugs {
		catalog.Models = append(catalog.Models, model{Slug: slug, Levels: []level{{"low"}, {"medium"}, {"high"}, {"xhigh"}, {"max"}}})
	}
	raw, err := json.Marshal(catalog)
	if err != nil {
		return err
	}
	dir := filepath.Join(homeDir, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "models_cache.json"), raw, 0o600)
}

// startFakes starts the world of the fake harnesses, units and hosts. Its
// environment is the desk's with the scenario's GitHub, so a unit or a
// host's coordinator reaches the same GitHub as the step that started it.
func (d *driver) startFakes() error {
	var err error
	d.fakes, err = harness.NewWorld(harness.Options{
		Given: d.s.Given.Harness, Hosts: d.s.Given.Hosts, Builder: d.s.Given.Builder, Env: d.stepEnv(deskCaller, nil), Artifacts: artifactsDir,
		Answer: func(program string, args []string, stdin, dir string, modeled bool) (string, string, int, bool) {
			r := d.world.Answer(CallRequest{Program: program, Args: args, Stdin: stdin, Dir: dir, Modeled: modeled})
			return r.Stdout, r.Stderr, r.Exit, r.Given
		},
	})
	if err != nil {
		return err
	}
	return d.fakes.Serve(harness.DefaultSocket)
}

// provisionHosts provisions the hosts against the desk's configuration,
// which then names each with the policy digest its host prepared.
func (d *driver) provisionHosts(ctx context.Context) error {
	hosts, err := harness.Provision(ctx, harness.Desk{
		Candidate: candidate, Fake: binDir + "/" + fakeBinary, Git: binDir + "/git", Config: configFile,
		Identity: deskIdentity, Orgs: d.orgs, Env: d.stepEnv(deskCaller, nil),
	}, d.s.Given.Hosts, d.repositories())
	if err != nil || len(hosts) == 0 {
		return err
	}
	overlay(d.cfg, map[string]any{"agents": map[string]any{"hosts": hosts}})
	overlay(d.cfg, d.s.Given.Desk.Config)
	raw, err := yaml.Marshal(d.cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(configFile, raw, 0o600); err != nil {
		return err
	}
	return d.seedHostPins(hosts)
}

// repositories are the scenario's repositories: given.repositories and
// given.github's, each once.
func (d *driver) repositories() []string { return d.s.repositoryNames() }

// startGitHub builds the scenario's GitHub from given.github over the
// origins, serves it on a loopback port and makes it answer gh.
func (d *driver) startGitHub(logMu *sync.Mutex) error {
	g := d.s.Given.GitHub
	if g == nil {
		return nil
	}
	log, err := os.Create(filepath.Join(artifactsDir, "github.jsonl"))
	if err != nil {
		return err
	}
	d.githubLog = log
	srv, err := fakegithub.New(g, fakegithub.Options{
		// The world's clock: budgets reset and fault windows close as
		// advance moves it, and reset times agree with the candidate's.
		Clock:   d.clock.Now,
		Origins: &fakegithub.Origins{Git: binDir + "/git", Root: originsDir, Env: env(nil)},
		Log: writerFunc(func(b []byte) (int, error) {
			logMu.Lock()
			defer logMu.Unlock()
			return log.Write(b)
		}),
		BoardOwner: d.boardOwner, BoardNumber: d.boardNumber, Repositories: d.repositories(),
	})
	if err != nil {
		return fmt.Errorf("given.github: %w", err)
	}
	if err := srv.Listen(); err != nil {
		return fmt.Errorf("the scenario's GitHub: %w", err)
	}
	d.github = srv
	d.world.Handle("gh", func(req CallRequest) (CallResponse, bool, string) {
		res := srv.GH(context.Background(), fakegithub.GHCall{Args: req.Args, Stdin: req.Stdin, Dir: req.Dir, Env: req.Env})
		return CallResponse{Stdout: res.Stdout, Stderr: res.Stderr, Exit: res.Exit}, res.Handled, res.Reason
	})
	return nil
}

// stepEnv is the environment of a step's candidate: the allowlist, the
// scenario's GitHub with the step as its caller, the scenario's tools and
// the step's own.
func (d *driver) stepEnv(caller string, extra map[string]string) []string {
	vars := map[string]string{}
	if d.github != nil {
		maps.Copy(vars, d.github.Env(caller))
	}
	if tools := d.s.Given.Tools; len(tools) > 0 {
		vars[ToolsEnv] = strings.Join(tools, ",")
	}
	maps.Copy(vars, extra)
	return env(vars)
}

// baseConfig is the desk every scenario starts from: one person, a
// two-slot local capacity, a board of example/1 and no claims, sync or
// background guards that would reach outside the world.
func baseConfig(orgs []string) map[string]any {
	return map[string]any{
		"stateDir": stateDir,
		"leaseDir": workDir + "/leases",
		"identity": map[string]any{"person": "scenario", "team": "scenario", "host": deskIdentity},
		"github":   map[string]any{"floor": -1, "allowedOrgs": orgs},
		"guide":    map[string]any{"person": "scenario"},
		"agents":   map[string]any{"mode": "background", "dir": repoDir, "staleAfter": "-1s"},
		"capacity": map[string]any{
			"ceiling": 2, "reviewHeadroom": 0, "availMinMiB": 1,
			"providers": map[string]any{"claude": map[string]any{"ceiling": 2}},
			"choices":   []any{map[string]any{"harness": "claude", "model": "sonnet"}},
		},
		// The machine's own pressure is no part of a scenario: a busy CI
		// runner must not hold a scenario's starts.
		"watch": map[string]any{"availMinMiB": 1, "loadMax": 1000000, "cpuPSIMax": 100, "psiMax": 100},
		"supervisor": map[string]any{
			"effort": "low", "workerBrief": workDir + "/worker.txt",
			"ownership": map[string]any{"authors": []any{"desk"}},
		},
		"board": map[string]any{
			"owner": "example", "project": 1, "tidyEvery": "-1s", "escalateEvery": "-1s",
			"order":  []any{map[string]any{"name": "ready", "status": []any{"Ready"}}},
			"claims": map[string]any{"off": true},
		},
		"disk": map[string]any{"checkouts": []any{repoDir}},
	}
}

// overlay lays src over dst, map by map; any other value replaces.
func overlay(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				overlay(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

// origin creates the bare origin of owner/name with one commit on main.
func origin(ctx context.Context, name string) error {
	bare := filepath.Join(originsDir, name+".git")
	seed, err := os.MkdirTemp(workDir, "seed-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(seed) }()
	for _, args := range [][]string{
		{"init", "--bare", "-b", "main", bare},
		{"-C", seed, "init", "-b", "main"},
		{"-C", seed, "commit", "--allow-empty", "-m", "initial commit"},
		{"-C", seed, "push", bare, "main"},
	} {
		c := exec.CommandContext(ctx, binDir+"/git", args...) //nolint:gosec // real git on the scenario's own paths
		c.Env = env(nil)
		if out, err := c.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
		}
	}
	return nil
}

// globals fills the parts of a view every step kind holds: the event log,
// the world's calls and the background processes.
func (d *driver) globals(v View) View {
	v.Events = func() ([]string, error) {
		if _, err := os.Stat(stateDir); err != nil {
			return nil, err
		}
		st, err := state.Open(stateDir)
		if err != nil {
			return nil, err
		}
		evs, err := st.Events(0, nil)
		if err != nil {
			return nil, err
		}
		verbs := make([]string, 0, len(evs))
		for _, e := range evs {
			verbs = append(verbs, e.Verb)
		}
		return verbs, nil
	}
	v.Calls = d.world.Count
	if d.github != nil {
		v.GitHub = d.github.Check
	}
	v.Worker = d.fakes.CheckWorker
	v.Log = d.backgroundLog
	v.Origin = func(repository, branch string) (bool, int, error) { return originBranch(repository, branch) }
	v.Process = func(name string) (bool, int, bool) {
		p, ok := d.procs[name]
		if !ok {
			return false, 0, false
		}
		running, exit := p.state()
		return running, exit, true
	}
	return v
}

func (d *driver) step(ctx context.Context, i int, st Step) StepResult {
	sr := StepResult{Index: i, Name: st.Name, Kind: st.Kind()}
	started := time.Now()
	v := View{Kind: sr.Kind}
	switch sr.Kind {
	case KindRun:
		sr.Args = st.Run.Args
		exit, stdout, stderr, err := d.runCandidate(ctx, i, st)
		if err != nil {
			sr.Failures = append(sr.Failures, err.Error())
		}
		v.Exit, v.Stdout, v.Stderr = &exit, &stdout, &stderr
		sr.Exit = &exit
	case KindBackground:
		sr.Args = st.Background.Args
		if err := d.start(st); err != nil {
			sr.Failures = append(sr.Failures, err.Error())
		}
	case KindStop:
		if p, ok := d.procs[st.Stop]; ok {
			p.stop()
		} else {
			sr.Failures = append(sr.Failures, fmt.Sprintf("%s was never started", st.Stop))
		}
	case KindWaitFor:
		sr.Failures = append(sr.Failures, d.waitFor(ctx, st.WaitFor)...)
	case KindGitHub, KindFault:
		var err error
		switch {
		case d.github == nil:
			err = errors.New("the scenario has no given.github")
		case sr.Kind == KindGitHub:
			err = d.github.Run(st.GitHub)
		default:
			err = d.github.RunFault(st.Fault)
		}
		if err != nil {
			sr.Failures = append(sr.Failures, err.Error())
		}
	case KindAdvance:
		var failures []string
		sr.Args, failures = d.advance(ctx, st.Advance)
		sr.Failures = append(sr.Failures, failures...)
	case KindHost:
		if err := d.fakes.ApplyHostStep(*st.Host); err != nil {
			sr.Failures = append(sr.Failures, err.Error())
		}
	}
	dur := time.Since(started)
	sr.Duration = dur
	v.Duration = &dur
	v = d.globals(v)
	for j, e := range st.Expect {
		if err := Check(e, v); err != nil {
			sr.Failures = append(sr.Failures, fmt.Sprintf("expect[%d] %s: %v", j, e.Kind(), err))
		}
	}
	d.last = v
	if st.Name != "" {
		d.views[st.Name] = v
	}
	return sr
}

func (d *driver) runCandidate(ctx context.Context, i int, st Step) (exit int, stdout, stderr string, err error) {
	c := exec.CommandContext(ctx, binaryOf(st.Run), st.Run.Args...) //nolint:gosec // the candidate, or a release build, with the scenario's arguments
	c.Dir = repoDir
	c.Env = d.stepEnv(cmpOr(st.Name, fmt.Sprintf("step-%d", i)), st.Run.Env)
	c.Stdin = strings.NewReader(st.Run.Stdin)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = 5 * time.Second
	var out, errOut bytes.Buffer
	c.Stdout, c.Stderr = &out, &errOut
	runErr := c.Run()
	stdout, stderr = out.String(), errOut.String()
	base := filepath.Join(artifactsDir, fmt.Sprintf("%02d-%s", i, cmpOr(st.Name, KindRun)))
	_ = os.WriteFile(base+".stdout", out.Bytes(), 0o600)
	_ = os.WriteFile(base+".stderr", errOut.Bytes(), 0o600)
	var ee *exec.ExitError
	switch {
	case runErr == nil:
		return 0, stdout, stderr, nil
	case ctx.Err() != nil:
		return -1, stdout, stderr, fmt.Errorf("beekeeper %s: killed at the scenario's timeout %s", strings.Join(st.Run.Args, " "), d.s.Timeout)
	case errors.As(runErr, &ee):
		return ee.ExitCode(), stdout, stderr, nil
	default:
		return -1, stdout, stderr, fmt.Errorf("beekeeper %s: %w", strings.Join(st.Run.Args, " "), runErr)
	}
}

func (d *driver) start(st Step) error {
	log, err := os.Create(filepath.Join(artifactsDir, st.Name+".log"))
	if err != nil {
		return err
	}
	c := exec.Command(binaryOf(st.Background), st.Background.Args...) //nolint:gosec // the candidate, or a release build, with the scenario's arguments
	c.Dir = repoDir
	c.Env = d.stepEnv(st.Name, st.Background.Env)
	c.Stdin = strings.NewReader(st.Background.Stdin)
	c.Stdout, c.Stderr = log, log
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		_ = log.Close()
		return err
	}
	p := &proc{cmd: c, started: time.Now(), done: make(chan struct{})}
	d.procs[st.Name] = p
	go func() {
		err := c.Wait()
		_ = log.Close()
		p.mu.Lock()
		p.exited = true
		var ee *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &ee):
			p.exit = ee.ExitCode()
		default:
			p.exit = -1
		}
		p.mu.Unlock()
		close(p.done)
	}()
	return nil
}

// waitFor polls the expectations until all hold, the wait's timeout or the
// scenario's: the failures are the last poll's.
func (d *driver) waitFor(ctx context.Context, w *WaitFor) []string {
	interval := w.Interval.Duration
	if interval == 0 {
		interval = 100 * time.Millisecond
	}
	deadline := time.Now().Add(w.Timeout.Duration)
	for {
		v := d.globals(View{Kind: KindWaitFor})
		var failures []string
		for j, e := range w.Expect {
			if err := Check(e, v); err != nil {
				failures = append(failures, fmt.Sprintf("wait_for[%d] %s: %v", j, e.Kind(), err))
			}
		}
		if len(failures) == 0 {
			return nil
		}
		if time.Now().Add(interval).After(deadline) || ctx.Err() != nil {
			return append(failures, fmt.Sprintf("still failing after %s", w.Timeout))
		}
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
}

// originBranch reads the branches of a repository's bare origin that match
// pattern whole: whether one is there, and how many commits the first is
// ahead of the origin's main.
func originBranch(repository, pattern string) (exists bool, ahead int, err error) {
	re, err := regexp.Compile(`^(?:` + pattern + `)$`)
	if err != nil {
		return false, 0, err
	}
	bare := filepath.Join(originsDir, repository+".git")
	git := func(args ...string) (string, error) {
		c := exec.Command(binDir+"/git", append([]string{"--git-dir", bare}, args...)...) //nolint:gosec // real git on the scenario's own origin
		c.Env = env(nil)
		out, err := c.Output()
		return strings.TrimSpace(string(out)), err
	}
	out, err := git("for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if err != nil {
		return false, 0, err
	}
	for _, branch := range strings.Fields(out) {
		if !re.MatchString(branch) {
			continue
		}
		count, err := git("rev-list", "--count", "refs/heads/main..refs/heads/"+branch)
		if err != nil {
			return true, 0, err
		}
		_, err = fmt.Sscanf(count, "%d", &ahead)
		return true, ahead, err
	}
	return false, 0, nil
}
