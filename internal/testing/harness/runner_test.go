package harness

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/settle"
)

// Stub tools exist only in the harness's own test binary; they let the runner be tested without an
// engine.
const (
	toolStubEcho = "stub_echo"
	toolStubFail = "stub_fail"
)

func init() {
	Register(toolStubEcho, Tool{Args: []string{"value", argSinceStep}, Run: func(_ context.Context, w *World, a map[string]any) (any, error) {
		if _, err := w.markOf(a); err != nil {
			return nil, err
		}
		return map[string]any{"args": a}, nil
	}})
	Register("stub_counter", Tool{Args: []string{"key"}, Run: func(_ context.Context, w *World, a map[string]any) (any, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.deliveries++
		return map[string]any{"n": w.deliveries}, nil
	}})
	Register("stub_block", Tool{Args: []string{}, Run: func(ctx context.Context, _ *World, _ map[string]any) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	Register(toolStubFail, Tool{Args: []string{"message"}, Run: func(_ context.Context, _ *World, a map[string]any) (any, error) {
		return nil, errors.New(str(a, "message"))
	}})
	Register("stub_busy", Tool{Args: []string{"for"}, Run: func(_ context.Context, w *World, a map[string]any) (any, error) {
		d, _ := time.ParseDuration(str(a, "for"))
		done := w.Tracker.Begin("stub work")
		time.AfterFunc(d, done)
		return map[string]any{}, nil
	}})
}

// stubGitHub answers github_calls with a fixed number of mutating calls.
type stubGitHub struct {
	mutating int
	marks    []string
}

func (s *stubGitHub) Do(_ context.Context, tool string, _ map[string]any) (any, error) {
	if tool != "github_calls" {
		return nil, fmt.Errorf("stub: %s", tool)
	}
	return map[string]any{"count": 0, "mutating": s.mutating}, nil
}

func (s *stubGitHub) MarkStep(id string) { s.marks = append(s.marks, id) }

// stubScenario parses a scenario made of steps (YAML list items) and extra top-level keys.
func stubScenario(t *testing.T, extra, steps string) *Scenario {
	t.Helper()
	y := "name: board-stub\nconcept: board\ntags: [phase1]\ndescription: Given a stub When it runs Then it is checked.\npre_configuration: { fixture: base }\n" + extra + "steps:\n" + steps +
		"  - { id: zz-asserts, tool: stub_echo, expected: { json_path: { args: {} } } }\n"
	s, err := Parse([]byte(y), scenariosDir+"/board/board-stub.yaml", scenariosDir)
	if err != nil {
		t.Fatalf("%v\n%s", err, y)
	}
	return s
}

// stubWorld is a world without an engine: a fake clock, a tracker, a receiver registry and a stub
// fake GitHub.
func stubWorld(s *Scenario) (*World, *prometheus.Registry, *stubGitHub) {
	reg := prometheus.NewRegistry()
	gh := &stubGitHub{}
	return &World{
		Scenario: s, Clock: clock.NewFake(s.Pre.Clock), Tracker: settle.New(), GitHub: gh,
		Receiver: Receiver{Registry: reg}, stub: true, settleBound: 300 * time.Millisecond,
	}, reg, gh
}

func runStub(t *testing.T, extra, steps string) (*Result, *World) {
	t.Helper()
	s := stubScenario(t, extra, steps)
	w, _, _ := stubWorld(s)
	return Run(context.Background(), w), w
}

func TestRunPassesAndStopsAtFirstFailure(t *testing.T) {
	t.Parallel()
	res, _ := runStub(t, "", `
  - { id: a, tool: stub_echo, args: { value: 1 }, expected: { json_path: { args.value: 1 } } }
  - { id: b, tool: stub_echo, args: { value: 2 }, expected: { json_path: { args.value: 3 } } }
  - { id: c, tool: stub_echo, args: { value: 3 } }
`)
	if !res.Failed() || len(res.Steps) != 2 || !res.Steps[1].Failed() {
		t.Fatalf("want a failure at step b and no step c: %+v", res.Steps)
	}
	if r := res.Report(); !strings.Contains(r, `step 1 "b"`) || !strings.Contains(r, "args.value: want 3, got 2") {
		t.Errorf("report doesn't name the step and the mismatch:\n%s", r)
	}
}

// TestEveryExpectationKindIsActedOn runs a step through the runner with each expectation kind set alone,
// to a value the result doesn't meet: every kind must fail it, so none passes vacuously. A new field on
// Expected fails this test until it gets a probe.
func TestEveryExpectationKindIsActedOn(t *testing.T) {
	t.Parallel()
	probes := map[string]struct{ tool, expected string }{
		"success":        {toolStubFail, "{ success: true }"},
		"error_contains": {toolStubFail, "{ success: false, error_contains: [absent] }"},
		"json_path":      {toolStubEcho, "{ json_path: { args.value: 2 } }"},
		"json_path_max":  {toolStubEcho, "{ json_path_max: { args.value: 0 } }"},
		"json_path_min":  {toolStubEcho, "{ json_path_min: { args.value: 5 } }"},
		"contains":       {toolStubEcho, "{ contains: [absent] }"},
		"not_contains":   {toolStubEcho, "{ not_contains: [value] }"},
		// wait_for_state is acted on when it makes a failing step pass (TestWaitForState); alone it
		// must still fail a step whose other expectations never hold.
		"wait_for_state": {toolStubEcho, "{ wait_for_state: 200ms, json_path: { args.value: 2 } }"},
	}
	typ := reflect.TypeOf(Expected{})
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.Split(typ.Field(i).Tag.Get("yaml"), ",")[0]
		p, ok := probes[tag]
		if !ok {
			t.Errorf("expectation kind %q has no probe", tag)
			continue
		}
		res, _ := runStub(t, "", fmt.Sprintf("  - { id: s, tool: %s, args: { %s: 1 }, expected: %s }\n  - { id: t, tool: stub_echo, expected: { json_path: { args: {} } } }\n", p.tool, map[string]string{toolStubFail: "message", toolStubEcho: "value"}[p.tool], p.expected))
		if !res.Steps[0].Failed() {
			t.Errorf("%s: a step that doesn't meet it passed", tag)
		}
	}
}

func TestWaitForState(t *testing.T) {
	t.Parallel()
	res, _ := runStub(t, "", "  - { id: s, tool: stub_counter, expected: { wait_for_state: 5s, json_path: { n: 3 } } }\n")
	if res.Failed() || res.Steps[0].Polls != 3 {
		t.Fatalf("want a pass on the third poll: %s polls=%d", res.Report(), res.Steps[0].Polls)
	}
	res, _ = runStub(t, "", "  - { id: s, tool: stub_counter, expected: { json_path: { n: 3 } } }\n")
	if !res.Failed() || res.Steps[0].Polls != 1 {
		t.Fatalf("without wait_for_state the step is judged once")
	}
	t0 := time.Now()
	res, _ = runStub(t, "", "  - { id: s, tool: stub_counter, expected: { wait_for_state: 150ms, json_path: { n: 0 } } }\n")
	if !res.Failed() || time.Since(t0) > 2*time.Second {
		t.Fatalf("wait_for_state didn't give up in time")
	}
}

// TestEveryStepFieldIsActedOn proves each field of Step changes what the runner does.
func TestEveryStepFieldIsActedOn(t *testing.T) {
	t.Parallel()
	checks := map[string]func(t *testing.T){
		"id": func(t *testing.T) {
			res, w := runStub(t, "", "  - { id: first, tool: stub_echo }\n  - { id: second, tool: stub_echo, args: { since_step: first }, expected: { json_path: { args.since_step: first } } }\n")
			if res.Failed() || w.marks["first"] == nil || w.GitHub.(*stubGitHub).marks[1] != "second" || w.GitHub.(*stubGitHub).marks[0] != "first" {
				t.Errorf("step ids aren't marked: %s", res.Report())
			}
		},
		"description": func(t *testing.T) {
			res, _ := runStub(t, "", "  - { id: s, tool: stub_fail, description: the reason it matters, args: { message: x } }\n")
			if !strings.Contains(res.Report(), "the reason it matters") {
				t.Errorf("a failure doesn't show the step's description:\n%s", res.Report())
			}
		},
		"tool": func(t *testing.T) {
			res, _ := runStub(t, "", "  - { id: s, tool: stub_fail, args: { message: boom } }\n")
			if !res.Failed() || !strings.Contains(res.Report(), "boom") {
				t.Errorf("the tool wasn't run")
			}
		},
		"args": func(t *testing.T) {
			res, _ := runStub(t, "", "  - { id: s, tool: stub_echo, args: { value: 7 }, expected: { json_path: { args.value: 7 } } }\n")
			if res.Failed() {
				t.Errorf("args weren't passed: %s", res.Report())
			}
		},
		"expected": func(t *testing.T) {
			res, _ := runStub(t, "", "  - { id: s, tool: stub_echo, args: { value: 7 }, expected: { json_path: { args.value: 8 } } }\n")
			if !res.Failed() {
				t.Errorf("expected wasn't evaluated")
			}
		},
		"timeout": func(t *testing.T) {
			t0 := time.Now()
			res, _ := runStub(t, "", "  - { id: s, tool: stub_block, timeout: 1s }\n  - { id: t, tool: stub_echo, expected: { json_path: { args: {} } } }\n")
			if !res.Failed() || time.Since(t0) > 15*time.Second {
				t.Errorf("timeout wasn't applied")
			}
		},
	}
	typ := reflect.TypeOf(Step{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		check, ok := checks[tag]
		if !ok {
			t.Errorf("step field %q has no check", tag)
			continue
		}
		t.Run(tag, func(t *testing.T) { t.Parallel(); check(t) })
	}
}

// TestEveryScenarioFieldIsActedOn proves each field of Scenario is used by the loader, the filter or
// the runner.
func TestEveryScenarioFieldIsActedOn(t *testing.T) {
	t.Parallel()
	checks := map[string]func(t *testing.T){
		"name":    func(t *testing.T) { loaderRejects(t, "name: board-example", "name: board-x", "file name") },
		"concept": func(t *testing.T) { loaderRejects(t, "concept: board", "concept: wait", "directory") },
		"tags":    func(t *testing.T) { loaderRejects(t, "tags: [phase1]", "tags: []", "phase1") },
		"description": func(t *testing.T) {
			loaderRejects(t, "description: Given a board When it is read Then it is there.\n", "", "description")
		},
		"pre_configuration": func(t *testing.T) {
			loaderRejects(t, "  fixture: base\n", "  fixture: base\n  clock: never\n", "clock")
		},
		"expect_violations": func(t *testing.T) { postCheckWithViolation(t) },
		"post_checks":       func(t *testing.T) { postCheckOverrides(t) },
		"steps":             func(t *testing.T) { loaderRejects(t, "steps:\n", "steps: []\nzzz:\n", "zzz") },
	}
	typ := reflect.TypeOf(Scenario{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if tag == "-" {
			continue
		}
		check, ok := checks[tag]
		if !ok {
			t.Errorf("scenario field %q has no check", tag)
			continue
		}
		t.Run(tag, func(t *testing.T) { t.Parallel(); check(t) })
	}
	// filters act on concept and tags
	all, err := LoadDir(scenariosDir)
	if err != nil {
		t.Fatal(err)
	}
	if n, want := len(Filter(all, "webhook", "")), scenarioFiles(t, "webhook"); n != want || n == 0 {
		t.Errorf("concept filter: %d, want %d", n, want)
	}
	if n := len(Filter(all, "", "learning-api-budget")); n == 0 || n == len(all) {
		t.Errorf("tag filter: %d", n)
	}
}

func loaderRejects(t *testing.T, from, to, want string) {
	t.Helper()
	if !strings.Contains(valid, from) {
		t.Fatalf("%q not in the valid scenario", from)
	}
	_, err := parseValid(t, strings.Replace(valid, from, to, 1))
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("want an error containing %q, got %v", want, err)
	}
}

func violations(reg *prometheus.Registry, inv string, v float64) {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "engine_invariant_violations"}, []string{"invariant"})
	reg.MustRegister(g)
	g.WithLabelValues(inv).Set(v)
	g.WithLabelValues("pending-run-placed").Set(0)
}

func postCheckWithViolation(t *testing.T) {
	for _, c := range []struct {
		extra string
		fail  bool
	}{
		{"", true},
		{"expect_violations: [wait-within-deadline]\n", false},
		{"expect_violations: [leader-decides]\n", true},
	} {
		s := stubScenario(t, c.extra, "  - { id: s, tool: stub_echo, expected: { json_path: { args: {} } } }\n")
		w, reg, _ := stubWorld(s)
		violations(reg, "wait-within-deadline", 1)
		res := Run(context.Background(), w)
		if res.Failed() != c.fail {
			t.Errorf("%q: failed=%v, want %v: %v", c.extra, res.Failed(), c.fail, res.Post)
		}
	}
}

func postCheckOverrides(t *testing.T) {
	steps := "  - { id: s, tool: stub_echo, expected: { json_path: { args: {} } } }\n"
	// no_reconcile_errors
	for _, c := range []struct {
		extra string
		fail  bool
	}{{"", true}, {"post_checks: { no_reconcile_errors: false }\n", false}} {
		s := stubScenario(t, c.extra, steps)
		w, reg, _ := stubWorld(s)
		ce := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "engine_reconcile_errors_total"}, []string{"controller"})
		reg.MustRegister(ce)
		ce.WithLabelValues("item").Inc()
		if res := Run(context.Background(), w); res.Failed() != c.fail {
			t.Errorf("no_reconcile_errors %q: failed=%v: %v", c.extra, res.Failed(), res.Post)
		}
	}
	// invariants_hold
	for _, c := range []struct {
		extra string
		fail  bool
	}{{"", true}, {"post_checks: { invariants_hold: false }\n", false}} {
		s := stubScenario(t, c.extra, steps)
		w, reg, _ := stubWorld(s)
		violations(reg, "leader-decides", 1)
		if res := Run(context.Background(), w); res.Failed() != c.fail {
			t.Errorf("invariants_hold %q: failed=%v: %v", c.extra, res.Failed(), res.Post)
		}
	}
	// settled
	for _, c := range []struct {
		extra string
		fail  bool
	}{{"", true}, {"post_checks: { settled: false }\n", false}} {
		s := stubScenario(t, c.extra, "  - { id: s, tool: stub_busy, args: { for: 1s } }\n  - { id: t, tool: stub_echo, expected: { json_path: { args: {} } } }\n")
		w, _, _ := stubWorld(s)
		if res := Run(context.Background(), w); res.Failed() != c.fail {
			t.Errorf("settled %q: failed=%v: %s", c.extra, res.Failed(), res.Report())
		}
	}
	// no_github_writes, always
	s := stubScenario(t, "", steps)
	w, _, gh := stubWorld(s)
	gh.mutating = 1
	if res := Run(context.Background(), w); !res.Failed() || !strings.Contains(strings.Join(res.Post, " "), "no_github_writes") {
		t.Errorf("no_github_writes: %v", res.Post)
	}
}

func TestSettleWaitsForWorkAndDueTimers(t *testing.T) {
	t.Parallel()
	s := stubScenario(t, "", "  - { id: s, tool: settle, expected: { json_path: { settled: true } } }\n")
	w, _, _ := stubWorld(s)
	w.settleBound = 2 * time.Second
	var fired atomic.Int32
	done := w.Tracker.Begin("work")
	time.AfterFunc(30*time.Millisecond, func() {
		// The work schedules a timer due now; Settle must fire it.
		w.Clock.AfterFunc(0, func() { fired.Add(1) })
		done()
	})
	if err := w.Settle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fired.Load() != 1 {
		t.Errorf("due timer not fired")
	}
	if ok, _ := w.Tracker.Idle(); !ok {
		t.Errorf("not idle after Settle")
	}
}

func TestSettleFailureReportsWhatIsBusy(t *testing.T) {
	t.Parallel()
	s := stubScenario(t, "", "  - { id: s, tool: settle, expected: { json_path: { settled: true } } }\n")
	w, _, _ := stubWorld(s)
	w.settleBound = 50 * time.Millisecond
	done := w.Tracker.Begin("reconcile Item giantswarm-roadmap-101")
	defer done()
	err := w.Settle(context.Background())
	var se *SettleError
	if !errors.As(err, &se) || !strings.Contains(err.Error(), "reconcile Item giantswarm-roadmap-101") || !strings.Contains(se.Dump, "goroutines") {
		t.Fatalf("settle error %v", err)
	}
}

// TestAdvanceClockStepsTimerToTimer checks that every timer due within the span fires at its own
// instant, after the work of earlier instants has settled, including timers scheduled on the way.
func TestAdvanceClockStepsTimerToTimer(t *testing.T) {
	t.Parallel()
	s := stubScenario(t, "", "  - { id: s, tool: clock_advance, args: { by: 5m } }\n")
	w, _, _ := stubWorld(s)
	w.settleBound = 2 * time.Second
	start := w.Clock.Now()
	var mu sync.Mutex
	var log []string
	note := func(what string) {
		mu.Lock()
		log = append(log, fmt.Sprintf("%s@%s", what, w.Clock.Now().Sub(start)))
		mu.Unlock()
	}
	w.Clock.AfterFunc(time.Minute, func() {
		note("t1")
		done := w.Tracker.Begin("async")
		go func() {
			time.Sleep(20 * time.Millisecond)
			note("async-done")
			done()
		}()
	})
	w.Clock.AfterFunc(3*time.Minute, func() {
		note("t3")
		w.Clock.AfterFunc(time.Minute, func() { note("t4") })
	})
	w.Clock.AfterFunc(10*time.Minute, func() { note("t10") })
	res := Run(context.Background(), w)
	if res.Failed() {
		t.Fatal(res.Report())
	}
	want := "t1@1m0s async-done@1m0s t3@3m0s t4@4m0s"
	if got := strings.Join(log, " "); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if got := w.Clock.Now().Sub(start); got != 5*time.Minute {
		t.Errorf("clock at +%s", got)
	}
}

// TestEveryContractToolIsRegistered checks the registry against the step tools of contract §4.2.
func TestEveryContractToolIsRegistered(t *testing.T) {
	t.Parallel()
	contract := []string{
		"github_issue_create", "github_issue_edit", "github_board_set", "github_board_add", "github_board_archive",
		"github_board_remove", "github_sub_issue", "github_dependency", "github_pr_open", "github_pr_push",
		"github_pr_review", "github_pr_merge", "github_pr_close", "github_check_run", "github_status",
		"github_installation_repos", "github_fault", "webhook_post", "clock_advance", "jev_set", "config_apply",
		"engine_fault", "postgres_fault", "settle", "get_item", "get_change", "get_team", "get_person",
		"get_installation", "get_budget", "list_items", "events", "decisions", "metric", "github_calls",
		"jev_calls", "reconciles", "object_writes", "replay",
	}
	var registered []string
	for _, n := range Tools() {
		if !strings.HasPrefix(n, "stub_") {
			registered = append(registered, n)
		}
	}
	for _, n := range contract {
		if _, ok := Lookup(n); !ok {
			t.Errorf("%s is not registered", n)
		}
	}
	if len(registered) != len(contract) {
		t.Errorf("registered %d tools, the contract has %d: %v", len(registered), len(contract), registered)
	}
}

// TestSettleFiresDueTimersWhileBusy: work that waits for a timer due now must not deadlock the settle.
func TestSettleFiresDueTimersWhileBusy(t *testing.T) {
	t.Parallel()
	s := stubScenario(t, "", "  - { id: s, tool: settle, expected: { json_path: { settled: true } } }\n")
	w, _, _ := stubWorld(s)
	w.settleBound = 2 * time.Second
	done := w.Tracker.Begin("start-up waiting for its first timer")
	w.Clock.AfterFunc(0, done)
	if err := w.Settle(context.Background()); err != nil {
		t.Fatal(err)
	}
}
