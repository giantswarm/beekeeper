//go:build scenario

package scenario

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/scenario/harness"
)

const validScenario = `
name: valid
flow: smoke
when:
  - name: version
    run: {args: [version]}
then:
  - exit: 0
`

// The loader is strict: what muster learned, assertion-free or mistyped
// scenarios pass vacuously otherwise.
func TestParseRejects(t *testing.T) {
	if _, err := Parse("valid.yaml", []byte(validScenario)); err != nil {
		t.Fatalf("the valid scenario: %v", err)
	}
	for _, tc := range []struct{ name, doc, want string }{
		{"unknown top-level key", validScenario + "assert: [exit]\n", "field assert not found"},
		{"unknown key in a step", strings.Replace(validScenario, "run: {args: [version]}", "run: {args: [version], argz: [x]}", 1), "field argz not found"},
		{"unknown step", strings.Replace(validScenario, "run: {args: [version]}", "sleep: 1s", 1), "field sleep not found"},
		{"unknown expectation", strings.Replace(validScenario, "exit: 0", "exit_code: 0", 1), "field exit_code not found"},
		{"no then", strings.Replace(validScenario, "then:\n  - exit: 0\n", "", 1), "no then"},
		{"empty then", strings.Replace(validScenario, "then:\n  - exit: 0\n", "then: []\n", 1), "no then"},
		{"no when", "name: x\nflow: smoke\nthen:\n  - calls: {max: 0}\n", "no when"},
		{"unbounded wait_for", strings.Replace(validScenario, "run: {args: [version]}", "wait_for: {expect: [{calls: {min: 1}}]}", 1), "without a timeout is unbounded"},
		{"wait_for beyond the scenario", "name: x\nflow: smoke\ntimeout: 5s\nwhen:\n  - wait_for: {timeout: 10s, expect: [{calls: {min: 1}}]}\nthen:\n  - calls: {min: 1}\n", "exceeds the scenario's"},
		{"two step kinds", strings.Replace(validScenario, "run: {args: [version]}", "run: {args: [version]}\n    stop: x", 1), "exactly one of run, background, stop, wait_for, advance, github, fault and host"},
		{"expectation without a kind", strings.Replace(validScenario, "exit: 0", "step: version", 1), "exactly one of exit"},
		{"two expectation kinds", strings.Replace(validScenario, "exit: 0", "{exit: 0, duration: {max: 1s}}", 1), "exactly one of exit"},
		{"expectation the step does not observe", "name: x\nflow: smoke\nwhen:\n  - {name: b, background: {args: [watch]}}\nthen:\n  - exit: 0\n", "exit is not observed for a background step"},
		{"then names no step", strings.Replace(validScenario, "exit: 0", "{step: nope, exit: 0}", 1), `step "nope" names no step`},
		{"stop of no background", strings.Replace(validScenario, "run: {args: [version]}", "stop: loop", 1), "names no earlier background step"},
		{"unknown fake", strings.Replace(validScenario, "when:", "given:\n  calls: [{program: curl, respond: {exit: 0}}]\nwhen:", 1), `program "curl"`},
		{"bad pending", validScenario + "pending: example/repo#1\n", "the full URL of the open issue"},
		{"bad duration", validScenario + "timeout: soon\n", "invalid duration"},
		{"two documents", validScenario + "---\n" + validScenario, "one scenario per file"},
		{"json_path without a check", strings.Replace(validScenario, "exit: 0", "json_path: {path: a.b}", 1), "exactly one of equals, exists and len"},
		{"builder with an unknown tool", strings.Replace(validScenario, "when:", "given:\n  builder: {tools: [cargo]}\nwhen:", 1), `"cargo" is none of go, golangci-lint`},
		{"generated desk without a repository", strings.Replace(validScenario, "when:", "given:\n  state: {generate: {agents: 3}}\nwhen:", 1), "which has none"},
		{"state that seeds nothing", strings.Replace(validScenario, "when:", "given:\n  repositories: [{name: example/app}]\n  state: {}\nwhen:", 1), "given.state without generate"},
		{"generated desk with an unknown key", strings.Replace(validScenario, "when:", "given:\n  repositories: [{name: example/app}]\n  state: {generate: {workers: 3}}\nwhen:", 1), "field workers not found"},
		{"github step without given.github", strings.Replace(validScenario, "run: {args: [version]}", "github: {merge: {repo: example/app, pull: 1}}", 1), "a github step needs given.github"},
		{"github step with two actions", strings.Replace(validScenario, "run: {args: [version]}", "github: {merge: {repo: example/app, pull: 1}, close: {repo: example/app, pull: 1}}", 1), "exactly one of push, check, review, merge, close, comment and budget"},
		{"fault step that neither arms nor clears", strings.Replace(validScenario, "run: {args: [version]}", "fault: {}", 1), "exactly one of arm and clear"},
		{"github expectation without given.github", strings.Replace(validScenario, "exit: 0", "github: {calls: {min: 1}}", 1), "a github expectation needs given.github"},
		{"github expectation without a kind", strings.Replace(validScenario, "exit: 0", "github: {}", 1), "exactly one of calls, budget, pull and issue"},
		{"unknown key in given.github", strings.Replace(validScenario, "when:", "given:\n  github: {repositorys: []}\nwhen:", 1), "field repositorys not found"},
		{"given.github that does not build", strings.Replace(validScenario, "when:", "given:\n  github: {repositories: [{name: example/app, issues: [{number: 1, parent: 9}]}]}\nwhen:", 1), "example/app#9 is no issue of the scenario"},
		{"worker script with an unknown outcome", strings.Replace(validScenario, "when:", "given:\n  harness:\n    workers: [{name: w, turns: [{outcome: explode}]}]\nwhen:", 1), `outcome "explode"`},
		{"worker script with an unknown key", strings.Replace(validScenario, "when:", "given:\n  harness:\n    workers: [{name: w, turns: [{steps: [{shell: ls}]}]}]\nwhen:", 1), "field shell not found"},
		{"worker step naming nothing saved", strings.Replace(validScenario, "when:", "given:\n  harness:\n    workers: [{turns: [{steps: [{git: [push, '${branch}']}]}]}]\nwhen:", 1), "${branch} is neither saved"},
		{"host without a repository", strings.Replace(validScenario, "when:", "given:\n  hosts: [{name: builder}]\nwhen:", 1), "given.repositories is empty"},
		{"host with an unknown fault", strings.Replace(validScenario, "when:", "given:\n  repositories: [{name: example/app}]\n  hosts: [{name: builder, fault: on-fire}]\nwhen:", 1), `fault "on-fire"`},
		{"host step of no host", strings.Replace(validScenario, "run: {args: [version]}", "host: {name: builder, clear: true}", 1), `host "builder" is none of given.hosts`},
		{"worker expectation without a check", strings.Replace(validScenario, "exit: 0", "worker: {name: w}", 1), "one of turns, outcome and running"},
		{"tool that is a fake already", strings.Replace(validScenario, "when:", "given:\n  tools: [gh]\nwhen:", 1), `"gh" is on the desk's PATH already`},
		{"tool that is the builder's kubectl", strings.Replace(validScenario, "when:", "given:\n  tools: [kubectl]\nwhen:", 1), `"kubectl" is on the desk's PATH already`},
		{"given call of a program that is no tool", strings.Replace(validScenario, "when:", "given:\n  calls: [{program: go, respond: {exit: 0}}]\nwhen:", 1), `program "go" is none of`},
		{"origin of no repository", strings.Replace(validScenario, "exit: 0", "origin: {repository: example/app, branch: work}", 1), "is none of given.repositories"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.name+".yaml", []byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// Every scenario of the suite loads, in the directory of its flow.
func TestScenariosLoad(t *testing.T) {
	all, err := LoadDir("scenarios")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range all {
		names[s.Flow+"/"+s.Name] = true
	}
	for _, want := range []string{
		"smoke/version", "smoke/capacity-json-empty-desk", "smoke/board-next-fake-board",
		"harness/claude-worker-parks-on-its-pull", "harness/codex-worker-parks-on-its-pull", "harness/agy-worker-parks-on-its-pull",
		"harness/omp-worker-parks-on-its-pull", "harness/execution-host-worker-publishes-its-branch", "harness/execution-host-faults",
		"harness/crashed-worker-is-retired-hung-worker-holds-its-slot", "harness/claude-turn-reports-usage-and-hits-the-limit",
	} {
		if !names[want] {
			t.Errorf("no scenario %s in %v", want, names)
		}
	}
	if got := Filter(all, []string{"smoke"}, []string{"board"}); len(got) != 1 || got[0].Name != "board-next-fake-board" {
		t.Errorf("filter smoke+board: %v", got)
	}
	if got := Filter(all, []string{"harness"}, []string{"hosts"}); len(got) != 2 {
		t.Errorf("filter harness+hosts: %v", got)
	}
	if got := Filter(all, []string{"no-such-flow"}, nil); len(got) != 0 {
		t.Errorf("filter of an unknown flow: %v", got)
	}
}

// A pending scenario is expected to fail; once it passes, the run fails
// and asks for the marker's removal. A harness failure is never pending.
func TestOutcome(t *testing.T) {
	const url = "https://github.com/giantswarm/beekeeper/issues/1"
	for _, tc := range []struct {
		name    string
		res     Result
		status  string
		failing bool
		problem string
	}{
		{"passes", Result{}, StatusPassed, false, ""},
		{"fails", Result{Failures: []string{"exit 1, want 0"}}, StatusFailed, true, "exit 1, want 0"},
		{"pending fails as expected", Result{Pending: url, Failures: []string{"exit 1, want 0"}}, StatusPending, false, ""},
		{"pending passes", Result{Pending: url}, StatusFailed, true, "remove the pending marker"},
		{"harness failure of a pending scenario", Result{Pending: url, Harness: "no sandbox"}, StatusFailed, true, "harness: no sandbox"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.res
			status, err := Outcome(&r)
			if status != tc.status || (err != nil) != tc.failing || (tc.problem != "" && !strings.Contains(r.Problem, tc.problem)) {
				t.Fatalf("Outcome = %s, %v (problem %q); want %s, failing %t, problem %q", status, err, r.Problem, tc.status, tc.failing, tc.problem)
			}
		})
	}
}

// The world answers a matching given call, logs every call in order, and
// an unanswered one fails without being silently answered.
func TestWorldAnswersAndLogsCalls(t *testing.T) {
	w, err := NewWorld([]Call{
		{Program: "gh", Args: []string{"api", "query=.*projectV2.*"}, Respond: Response{Stdout: "{}\n"}, Times: 1},
		{Program: "gh", Args: []string{"auth"}, Respond: Response{Stderr: "not logged in\n", Exit: 4}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "world.sock")
	if err := w.Serve(socket); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	ask := func(program string, args ...string) CallResponse {
		t.Helper()
		resp, err := Ask(socket, CallRequest{Program: program, Args: args})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if r := ask("gh", "api", "graphql", "-f", "query={\nprojectV2(number:1){id}}"); r.Exit != 0 || r.Stdout != "{}\n" {
		t.Fatalf("matching call: %+v", r)
	}
	if r := ask("gh", "auth", "status"); r.Exit != 4 || r.Stderr != "not logged in\n" {
		t.Fatalf("second answer: %+v", r)
	}
	// Times: 1 is spent, so the same call again is unanswered.
	if r := ask("gh", "api", "graphql", "-f", "query={projectV2(number:1){id}}"); r.Exit == 0 || !strings.Contains(r.Stderr, "no given call answers gh api graphql") {
		t.Fatalf("spent answer reused: %+v", r)
	}
	if r := ask("ssh", "host", "true"); r.Exit == 0 {
		t.Fatalf("unknown call answered: %+v", r)
	}
	calls := w.Calls()
	if len(calls) != 4 || calls[0].Answer != 0 || calls[1].Answer != 1 || calls[2].Answered || calls[3].Program != "ssh" {
		t.Fatalf("call log: %+v", calls)
	}
	un := w.Unanswered()
	if len(un) != 2 || un[0].Seq != 3 || un[1].Seq != 4 {
		t.Fatalf("unanswered: %+v", un)
	}
	n, err := w.Count("gh", []string{"graphql"})
	if err != nil || n != 2 {
		t.Fatalf("count of gh graphql calls: %d, %v", n, err)
	}
}

// One checker reads one view for every step kind: an expectation of what a
// step kind does not observe fails instead of passing.
func TestCheckOneViewForEveryStepKind(t *testing.T) {
	exit, out, dur := 0, `{"pick":{"ref":"example/app#1"},"items":[1,2]}`, 2*time.Second
	run := View{Kind: KindRun, Exit: &exit, Stdout: &out, Duration: &dur}
	wait := View{Kind: KindWaitFor, Duration: &dur}
	zero, two := 0, 2
	yes, no := true, false
	withWorld := run
	withWorld.Worker = func(e harness.WorkerExpect) error {
		if e.Name != "w" {
			return errors.New("no worker " + e.Name)
		}
		return nil
	}
	withWorld.Origin = func(_, branch string) (bool, int, error) { return branch == "work", 2, nil }
	for _, tc := range []struct {
		name string
		e    Expect
		v    View
		want string
	}{
		{"exit", Expect{Exit: &zero}, run, ""},
		{"exit of a wait", Expect{Exit: &zero}, wait, "exit is not observed for a wait_for step"},
		{"output of a wait", Expect{Output: &TextExpect{Contains: []string{"x"}}}, wait, "output is not observed"},
		{"json path", Expect{JSONPath: &JSONPathExpect{Path: "pick.ref", Equals: "example/app#1"}}, run, ""},
		{"json path mismatch", Expect{JSONPath: &JSONPathExpect{Path: "pick.ref", Equals: "example/app#2"}}, run, `"example/app#1", want "example/app#2"`},
		{"json path index", Expect{JSONPath: &JSONPathExpect{Path: "items[1]", Equals: 2}}, run, ""},
		{"json path len", Expect{JSONPath: &JSONPathExpect{Path: "items", Len: &two}}, run, ""},
		{"json path exists", Expect{JSONPath: &JSONPathExpect{Path: "pick.title", Exists: &yes}}, run, "exists false, want true"},
		{"duration", Expect{Duration: &DurationExpect{Max: Duration{time.Second}}}, wait, "took 2s, at most 1s"},
		{"events without a log", Expect{Events: &EventsExpect{Contains: []string{"agents.start"}}}, run, "events is not observed"},
		{"worker without a world", Expect{Worker: &harness.WorkerExpect{Name: "w", Turns: &two}}, run, "worker is not observed"},
		{"worker", Expect{Worker: &harness.WorkerExpect{Name: "w", Turns: &two}}, withWorld, ""},
		{"worker mismatch", Expect{Worker: &harness.WorkerExpect{Name: "other", Turns: &two}}, withWorld, "no worker other"},
		{"origin without a world", Expect{Origin: &OriginExpect{Repository: "example/app", Branch: "work"}}, run, "origin is not observed"},
		{"origin branch", Expect{Origin: &OriginExpect{Repository: "example/app", Branch: "work", Ahead: &two}}, withWorld, ""},
		{"origin branch missing", Expect{Origin: &OriginExpect{Repository: "example/app", Branch: "gone"}}, withWorld, "branch gone there false, want true"},
		{"origin branch that should be missing", Expect{Origin: &OriginExpect{Repository: "example/app", Branch: "work", Exists: &no}}, withWorld, "branch work there true, want false"},
		{"origin branch ahead", Expect{Origin: &OriginExpect{Repository: "example/app", Branch: "work", Ahead: &zero}}, withWorld, "2 commit(s) ahead of main, want 0"},
		{"output", Expect{Output: &TextExpect{Contains: []string{"pick"}, NotContains: []string{"error"}, Matches: `^\{"pick"`}}, run, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(tc.e, tc.v)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("Check = %v, want %q", err, tc.want)
			}
		})
	}
}

// A fake that models its program answers a call itself, and the world logs
// the call as answered by the model; a given call still wins, so a
// scenario can override any call, and an unmodeled call nothing answers
// stays unanswered.
func TestWorldLeavesAModeledCallToItsFake(t *testing.T) {
	w, err := NewWorld([]Call{{Program: "claude", Args: []string{"-p", "/usage"}, Respond: Response{Stderr: "Not logged in\n", Exit: 1}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := w.Answer(CallRequest{Program: "claude", Args: []string{"-p", "/usage", "--output-format", "json"}, Modeled: true}); !r.Given || r.Exit != 1 || r.Stderr != "Not logged in\n" {
		t.Fatalf("the given call does not win over the model: %+v", r)
	}
	if r := w.Answer(CallRequest{Program: "claude", Args: []string{"agents", "--json"}, Modeled: true}); r.Given || r.Exit != 0 || r.Stderr != "" {
		t.Fatalf("a modeled call: %+v", r)
	}
	if r := w.Answer(CallRequest{Program: "claude", Args: []string{"mcp", "list"}}); r.Given || r.Exit == 0 {
		t.Fatalf("an unmodeled call nothing answers: %+v", r)
	}
	calls := w.Calls()
	if len(calls) != 3 || calls[0].Modeled || calls[0].Answer != 0 || !calls[1].Answered || !calls[1].Modeled || calls[1].Answer != -1 || calls[2].Answered {
		t.Fatalf("call log: %+v", calls)
	}
	if un := w.Unanswered(); len(un) != 1 || un[0].Seq != 3 {
		t.Fatalf("unanswered: %+v", un)
	}
}
