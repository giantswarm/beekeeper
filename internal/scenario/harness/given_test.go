//go:build scenario

package harness

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func parseGiven(t *testing.T, doc string) Given {
	t.Helper()
	var g Given
	dec := yaml.NewDecoder(strings.NewReader(doc))
	dec.KnownFields(true)
	if err := dec.Decode(&g); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return g
}

const validGiven = `
usage:
  claude: {session: 40, week: 20}
models:
  agy: [gemini-3-pro]
workers:
  - name: app-.*
    harness: claude
    turns:
      - steps:
          - git: [push, origin, work]
          - gh: [pr, create, --head, work]
            save: pull
          - beekeeper: [agents, park, --on, '${pull}', desk review]
          - write: {path: '${dir}/note.txt', content: hello}
        tokens: {input: 10, output: 2}
      - outcome: crash
        exit: 3
`

// A script that cannot do what it says is refused when the scenario
// loads, not when its worker runs.
func TestGivenValidate(t *testing.T) {
	g := parseGiven(t, validGiven)
	if err := g.Validate(nil); err != nil {
		t.Fatalf("the valid block: %v", err)
	}
	for _, tc := range []struct{ name, doc, want string }{
		{"unknown harness", "workers: [{name: x, harness: gemini, turns: [{}]}]", `harness "gemini"`},
		{"unknown outcome", "workers: [{turns: [{outcome: explode}]}]", `outcome "explode"`},
		{"exit without a crash", "workers: [{turns: [{exit: 2}]}]", "exit is a crash's exit code"},
		{"no turns", "workers: [{name: x}]", "no turns"},
		{"bad name pattern", "workers: [{name: '(', turns: [{}]}]", "name:"},
		{"two step kinds", "workers: [{turns: [{steps: [{git: [status], gh: [auth]}]}]}]", "exactly one of beekeeper, git, gh, run and write"},
		{"empty step", "workers: [{turns: [{steps: [{dir: x}]}]}]", "exactly one of beekeeper, git, gh, run and write"},
		{"write without a path", "workers: [{turns: [{steps: [{write: {content: x}}]}]}]", "write without a path"},
		{"unsaved name", "workers: [{turns: [{steps: [{git: [push, '${branch}']}]}]}]", "${branch} is neither saved"},
		{"name saved later", "workers: [{turns: [{steps: [{git: [push, '${b}']}, {git: [x], save: b}]}]}]", "${b} is neither saved"},
		{"save over a builtin", "workers: [{turns: [{steps: [{git: [x], save: name}]}]}]", `save "name"`},
		{"host that is not given", "workers: [{host: builder, turns: [{}]}]", `host "builder"`},
		{"usage of a harness without a probe", "usage: {omp: {session: 1}}", `"omp" has no usage probe`},
		{"usage beyond 100", "usage: {claude: {session: 101}}", "percent between 0 and 100"},
		{"models of a harness that lists none", "models: {claude: [sonnet]}", `"claude" lists no models`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := parseGiven(t, tc.doc)
			err := g.Validate(nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one containing %q", err, tc.want)
			}
		})
	}
	// A script may name a host of the scenario, and the desk as local.
	g = parseGiven(t, "workers: [{host: builder, turns: [{}]}, {host: local, turns: [{}]}]")
	if err := g.Validate([]Host{{Name: "builder"}}); err != nil {
		t.Fatalf("hosts builder and local: %v", err)
	}
}

func TestExpand(t *testing.T) {
	vars := map[string]string{"pull": "https://github.com/example/app/pull/2", "dir": "/work/repo"}
	for in, want := range map[string]string{
		"${pull}":              "https://github.com/example/app/pull/2",
		"${dir}/body.md":       "/work/repo/body.md",
		"no names here":        "no names here",
		"$pull and ${unknown}": "$pull and ",
	} {
		if got := Expand(in, vars); got != want {
			t.Errorf("Expand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateHosts(t *testing.T) {
	repos := []string{"example/app"}
	if err := ValidateHosts([]Host{{Name: "builder"}, {Name: "old", Fault: OlderGo, Harnesses: []string{Claude, Codex}}}, repos); err != nil {
		t.Fatalf("valid hosts: %v", err)
	}
	for _, tc := range []struct {
		name  string
		hosts []Host
		repos []string
		want  string
	}{
		{"local is the desk", []Host{{Name: "local"}}, repos, "not local"},
		{"upper case", []Host{{Name: "Builder"}}, repos, "lower-case"},
		{"twice", []Host{{Name: "a"}, {Name: "a"}}, repos, "a second host"},
		{"unknown harness", []Host{{Name: "a", Harnesses: []string{"gemini"}}}, repos, `harness "gemini"`},
		{"unknown fault", []Host{{Name: "a", Fault: "on-fire"}}, repos, `fault "on-fire"`},
		{"repository not given", []Host{{Name: "a", Repositories: []string{"example/other"}}}, repos, "none of given.repositories"},
		{"nothing to serve", []Host{{Name: "a"}}, nil, "given.repositories is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateHosts(tc.hosts, tc.repos)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestHostStepValidate(t *testing.T) {
	hosts := []Host{{Name: "builder"}}
	for _, ok := range []HostStep{{Name: "builder", Fault: Unreachable}, {Name: "builder", Clear: true}, {Name: "builder", Fault: MissingTool, Slow: Duration{1}}} {
		if err := ok.Validate(hosts); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	for want, bad := range map[string]HostStep{
		"none of given.hosts":      {Name: "other", Clear: true},
		`fault "on-fire"`:          {Name: "builder", Fault: "on-fire"},
		"a host's provisioning":    {Name: "builder", Fault: OlderGo},
		"one of fault, slow and c": {Name: "builder"},
		"(clear alone)":            {Name: "builder", Clear: true, Fault: Unreachable},
	} {
		if err := bad.Validate(hosts); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: error %v, want one containing %q", bad, err, want)
		}
	}
}

func TestWorkerExpectValidate(t *testing.T) {
	one, yes := 1, true
	for _, ok := range []WorkerExpect{{Name: "a", Turns: &one}, {Outcome: Crash}, {Harness: Codex, Running: &yes}} {
		if err := ok.Validate(); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	for want, bad := range map[string]WorkerExpect{
		"one of turns, outcome and running": {Name: "a"},
		`outcome "explode"`:                 {Outcome: "explode"},
		`harness "gemini"`:                  {Harness: "gemini", Turns: &one},
		"name:":                             {Name: "(", Turns: &one},
	} {
		if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: error %v, want one containing %q", bad, err, want)
		}
	}
}

func TestMachineOf(t *testing.T) {
	for _, tc := range []struct {
		paths []string
		want  string
	}{
		{[]string{"/work/repo", "/work/home"}, LocalHost},
		{[]string{"/work/hosts/builder/workspaces/x/workspace", "/work/home"}, "builder"},
		{[]string{"/work/repo", "/work/hosts/old/home"}, "old"},
		{[]string{"/work/hosts", ""}, LocalHost},
		{nil, LocalHost},
	} {
		if got := MachineOf(tc.paths...); got != tc.want {
			t.Errorf("MachineOf(%v) = %q, want %q", tc.paths, got, tc.want)
		}
	}
}
