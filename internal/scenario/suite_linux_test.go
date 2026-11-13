//go:build scenario && linux

package scenario

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/scenario/harness"
)

// The suite's wall-time budget: the run fails above Budget and warns from
// Warn, so growth is seen before it breaks the budget.
const (
	Budget = 10 * time.Minute
	Warn   = 8 * time.Minute
)

// ReportEnv names the directory of the suite's report and artifacts;
// FlowEnv and TagEnv filter the scenarios (comma-separated).
const (
	ReportEnv = "BEEKEEPER_SCENARIO_REPORT"
	FlowEnv   = "BEEKEEPER_SCENARIO_FLOW"
	TagEnv    = "BEEKEEPER_SCENARIO_TAG"
)

var suiteStarted = time.Now()

// The test binary is also every fake (dispatching on argv[0]) and each
// scenario's driver inside its sandbox; beekeeper itself is the candidate,
// built once.
func TestMain(m *testing.M) {
	// The fake harnesses, units and ssh answer for themselves; every other
	// fake hands its call to the world.
	if code, served := harness.Main(); served {
		os.Exit(code)
	}
	if code, served := ServeFake(); served {
		os.Exit(code)
	}
	if len(os.Args) > 2 && os.Args[1] == SandboxFlag {
		os.Exit(EnterAndDrive(os.Args[2]))
	}
	code := m.Run()
	// The candidate is some hundred megabytes: a run does not leave it in
	// the temporary directory, which on a desk is memory.
	if built != "" {
		_ = os.RemoveAll(filepath.Dir(built))
	}
	os.Exit(code)
}

var (
	buildOnce sync.Once
	built     string
	buildErr  error
	// releases builds the release builds of given.releases beside the
	// candidate, each once.
	releases *ReleaseBuilder
)

// runner builds the candidate once per test binary and returns the runner;
// it refuses, failing the test, without namespaces.
func runner(t *testing.T) *Runner {
	t.Helper()
	if err := Namespaces(); err != nil {
		t.Fatal(err)
	}
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "beekeeper-scenario-candidate-")
		if err != nil {
			buildErr = err
			return
		}
		built = filepath.Join(dir, "beekeeper")
		releases = &ReleaseBuilder{Repo: filepath.Join("..", ".."), Dir: dir}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		start := time.Now()
		buildErr = Build(ctx, filepath.Join("..", ".."), built)
		fmt.Fprintf(os.Stderr, "scenario candidate built in %s\n", time.Since(start).Round(time.Millisecond))
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &Runner{Candidate: built, Fake: self, Artifacts: os.Getenv(ReportEnv), Release: releases.Build}
}

// TestScenarios runs every scenario under scenarios/ (filtered by
// BEEKEEPER_SCENARIO_FLOW and BEEKEEPER_SCENARIO_TAG) in parallel, each in
// its own sandbox, and writes the report.
func TestScenarios(t *testing.T) {
	all, err := LoadDir("scenarios")
	if err != nil {
		t.Fatal(err)
	}
	scenarios := Filter(all, SplitList(os.Getenv(FlowEnv)), SplitList(os.Getenv(TagEnv)))
	if len(scenarios) == 0 {
		t.Fatalf("no scenario matches flows %q and tags %q", os.Getenv(FlowEnv), os.Getenv(TagEnv))
	}
	r := runner(t)
	rep := &Report{Started: suiteStarted, Budget: Budget, Warn: Warn}
	var mu sync.Mutex
	t.Run("scenarios", func(t *testing.T) {
		for _, s := range scenarios {
			t.Run(s.Flow+"/"+s.Name, func(t *testing.T) {
				t.Parallel()
				res := r.Run(t.Context(), s, t.TempDir())
				status, err := Outcome(res)
				mu.Lock()
				rep.Results = append(rep.Results, res)
				mu.Unlock()
				for _, st := range res.Steps {
					t.Logf("step %d %s %s: %s", st.Index, st.Kind, st.Name, st.Duration.Round(time.Millisecond))
				}
				if err != nil {
					t.Fatalf("%s: %v", status, err)
				}
				if status == StatusPending {
					t.Logf("pending on %s: fails as expected", s.Pending)
				}
			})
		}
	})
	rep.Wall = time.Since(suiteStarted)
	switch {
	case rep.Wall > Budget:
		msg := fmt.Sprintf("FAIL: the scenario suite took %s, above its budget of %s", rep.Wall.Round(time.Second), Budget)
		rep.Problems = append(rep.Problems, msg)
		t.Error(msg)
	case rep.Wall >= Warn:
		msg := fmt.Sprintf("WARNING: the scenario suite took %s, %s from its budget of %s", rep.Wall.Round(time.Second), (Budget - rep.Wall).Round(time.Second), Budget)
		rep.Problems = append(rep.Problems, msg)
		t.Log(msg)
	}
	sortResults(rep.Results)
	rep.Count()
	var text strings.Builder
	if err := rep.Text(&text); err != nil {
		t.Error(err)
	}
	t.Log("\n" + text.String())
	if dir := os.Getenv(ReportEnv); dir != "" {
		if err := rep.Write(dir); err != nil {
			t.Errorf("report: %v", err)
		}
	}
}

// TestUnansweredCallFailsTheScenario runs a scenario whose candidate calls
// gh with nothing in the world to answer it: the call is logged and the
// scenario fails, although every assertion of its own holds.
func TestUnansweredCallFailsTheScenario(t *testing.T) {
	s := mustParse(t, `
name: unanswered-gh
flow: harness
when:
  - name: next
    run: {args: [board, next]}
then:
  - calls: {program: gh, min: 1}
`)
	res := runner(t).Run(t.Context(), s, t.TempDir())
	status, err := Outcome(res)
	if err == nil || status != StatusFailed {
		t.Fatalf("an unanswered call passed: %s %v %+v", status, err, res)
	}
	if res.Harness != "" {
		t.Fatalf("the harness failed instead of the scenario: %s", res.Harness)
	}
	if !strings.Contains(err.Error(), "unanswered call #1: gh ") {
		t.Fatalf("failure does not name the unanswered call: %v", err)
	}
}

// TestFileExpectation runs scenarios with file expectations on files under
// the work directory: the CPU profile the candidate writes where a step's
// BEEKEEPER_CPUPROFILE names it, and the desk's config.yaml. They are read
// inline, in a wait_for and in then. A file that is not there, is smaller
// than min_bytes, lacks what it should contain or is there when it should
// not be fails the scenario on an assertion, not in the harness.
func TestFileExpectation(t *testing.T) {
	holds := mustParse(t, `
name: file-holds
flow: harness
when:
  - name: profiled
    run: {args: [version], env: {BEEKEEPER_CPUPROFILE: /work/cpu.pprof}}
    expect:
      - file: {path: cpu.pprof, min_bytes: 1}
  - wait_for:
      timeout: 5s
      expect:
        - file: {path: config.yaml, contains: ['stateDir: /work/state'], matches: '^stateDir: /work/state$'}
then:
  - {step: profiled, exit: 0}
  - file: {path: cpu.pprof, min_bytes: 1}
  - file: {path: config.yaml, not_contains: [BEEKEEPER_CPUPROFILE]}
  - file: {path: repo/cpu.pprof, exists: false}
`)
	fails := mustParse(t, `
name: file-fails
flow: harness
when:
  - run: {args: [version]}
then:
  - file: {path: cpu.pprof}
  - file: {path: config.yaml, min_bytes: 100000000}
  - file: {path: config.yaml, contains: [no such line]}
  - file: {path: config.yaml, exists: false}
`)
	r := runner(t)
	t.Run("holds", func(t *testing.T) {
		t.Parallel()
		res := r.Run(t.Context(), holds, t.TempDir())
		if !res.Passed() {
			t.Fatalf("file expectations that hold failed: harness %q, failures %q", res.Harness, res.Failures)
		}
	})
	t.Run("fails on assertions", func(t *testing.T) {
		t.Parallel()
		res := r.Run(t.Context(), fails, t.TempDir())
		if res.Harness != "" {
			t.Fatalf("the harness failed instead of the expectations: %s", res.Harness)
		}
		failures := strings.Join(res.Failures, "\n")
		for _, want := range []string{
			"then[0] file: file cpu.pprof is not there",
			"then[1] file: file config.yaml has ",
			"bytes, at least 100000000",
			`then[2] file: file config.yaml lacks "no such line"`,
			"then[3] file: file config.yaml is there (",
		} {
			if !strings.Contains(failures, want) {
				t.Errorf("failures lack %q:\n%s", want, failures)
			}
		}
		if len(res.Failures) != 4 {
			t.Errorf("%d failures, want one per expectation:\n%s", len(res.Failures), failures)
		}
	})
}

func mustParse(t *testing.T, doc string) *Scenario {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "scenario.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
