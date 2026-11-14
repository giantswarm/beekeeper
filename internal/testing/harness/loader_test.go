package harness

import (
	"path/filepath"
	"strings"
	"testing"
)

const scenariosDir = "../scenarios"

// Lines of the valid scenario the loader cases edit, and the error of a field the schema lacks.
const (
	atSteps          = "steps:\n"
	atFixture        = "  fixture: base\n"
	atJSONPath       = "      json_path:"
	wantUnknownField = "unknown field"
)

// valid is a minimal valid scenario; the cases below break it one way at a time.
const valid = `name: board-example
concept: board
tags: [phase1]
description: Given a board When it is read Then it is there.
pre_configuration:
  fixture: base
steps:
  - id: first
    tool: get_item
    args: { ref: giantswarm/roadmap#101 }
    expected:
      json_path: { status.phase: Ready }
`

func parseValid(t *testing.T, yaml string) (*Scenario, error) {
	t.Helper()
	return Parse([]byte(yaml), scenariosDir+"/board/board-example.yaml", scenariosDir)
}

func TestLoadAllScenarios(t *testing.T) {
	t.Parallel()
	all, err := LoadDir(scenariosDir)
	if err != nil {
		t.Fatal(err)
	}
	if want := scenarioFiles(t, "*"); len(all) != want {
		t.Fatalf("loaded %d scenarios, want %d (one per file)", len(all), want)
	}
	for _, s := range all {
		if s.Pre == nil || s.Pre.Clock.IsZero() || s.Pre.GitHub == nil {
			t.Errorf("%s: pre_configuration not decoded", s.Name)
		}
	}
}

func TestLoaderAcceptsValid(t *testing.T) {
	t.Parallel()
	s, err := parseValid(t, valid)
	if err != nil {
		t.Fatal(err)
	}
	if s.Pre.Clock.Format("2006-01-02T15:04:05Z07:00") != "2026-11-16T09:00:00Z" {
		t.Errorf("clock %s", s.Pre.Clock)
	}
	if s.Pre.Engine.Replicas != 1 {
		t.Errorf("replicas %d", s.Pre.Engine.Replicas)
	}
}

func TestLoaderRejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, from, to, want string
	}{
		{"unknown top-level key", "concept: board\n", "concept: board\ncategory: behavioral\n", "field category not found"},
		{"unknown step key", "    tool: get_item\n", "    tool: get_item\n    retry: 3\n", "field retry not found"},
		{"unknown expectation key", atJSONPath, "      status_code: 200\n      json_path:", "field status_code not found"},
		{"missing description", "description: Given a board When it is read Then it is there.\n", "", "missing description"},
		{"description without Given/When/Then", "Given a board When it is read Then it is there.", "A board.", "Given/When/Then"},
		{"name not the file name", "name: board-example", "name: board-other", "file name"},
		{"concept not the directory", "concept: board", "concept: wait", "directory"},
		{"unknown concept", "concept: board", "concept: boards", "unknown concept"},
		{"missing phase1 tag", "tags: [phase1]", "tags: [startup]", "phase1"},
		{"unknown tool", "tool: get_item", "tool: get_items", "unknown tool"},
		{"unknown arg", "args: { ref: giantswarm/roadmap#101 }", "args: { ref: giantswarm/roadmap#101, name: x }", `has no arg "name"`},
		{"no assertion", "    expected:\n      json_path: { status.phase: Ready }\n", "", "asserts anything"},
		{"no steps", atSteps, "steps: []\nx:\n", "field x not found"},
		{"duplicate step id", atSteps, "steps:\n  - { id: first, tool: settle }\n", "duplicate id"},
		{"bad enum", atSteps, "steps:\n  - { id: f, tool: engine_fault, args: { kind: explode } }\n", "not in"},
		{"since_step not earlier", atSteps, "steps:\n  - { id: e, tool: events, args: { since_step: later } }\n  - { id: later, tool: settle }\n", "not an earlier step"},
		{"unknown event type", atSteps, "steps:\n  - { id: e, tool: events, args: { type: wait.begun } }\n", "unknown event type"},
		{"unknown metric", atSteps, "steps:\n  - { id: m, tool: metric, args: { name: engine_nope } }\n", "unknown metric"},
		{"unknown invariant label", atSteps, "steps:\n  - { id: m, tool: metric, args: { name: engine_invariant_violations, labels: { invariant: nope } } }\n", "unknown invariant label"},
		{"unknown expected violation", atSteps, "expect_violations: [nope]\nsteps:\n", "unknown invariant"},
		{"bad deliver", atSteps, "steps:\n  - { id: d, tool: github_pr_close, args: { ref: a/b#1, deliver: sometimes } }\n", "bad deliver"},
		{"bad deliver delay", atSteps, "steps:\n  - { id: d, tool: github_pr_close, args: { ref: a/b#1, deliver: { delay: soon } } }\n", "bad deliver delay"},
		{"bad clock_advance", atSteps, "steps:\n  - { id: c, tool: clock_advance, args: { by: a while } }\n", "bad duration"},
		{"clock_advance without by", atSteps, "steps:\n  - { id: c, tool: clock_advance }\n", "needs by"},
		{"bad wait_for_state", atJSONPath, "      wait_for_state: soon\n      json_path:", "wait_for_state"},
		{"bad timeout", "    tool: get_item\n", "    tool: get_item\n    timeout: forever\n", "bad timeout"},
		{"error_contains without success false", "      json_path: { status.phase: Ready }", "      error_contains: [nope]", "needs success: false"},
		{"success false with json_path", atJSONPath, "      success: false\n      json_path:", "only be combined"},
		{"json_path_max not a number", atJSONPath, "      json_path_max: { count: many }\n      json_path:", "not a number"},
		{"bad path", "status.phase: Ready", "status..phase: Ready", "empty segment"},
		{"no_github_writes off", atSteps, "post_checks: { no_github_writes: false }\nsteps:\n", "can't be switched off"},
		{"unknown post check", atSteps, "post_checks: { quiet: true }\nsteps:\n", "field quiet not found"},
		{"unknown pre_configuration key", atFixture, "  fixture: base\n  mocks: {}\n", `unknown key "mocks"`},
		{"other fixture", "fixture: base", "fixture: scale", "fixture must be base"},
		{"unknown installation field", atFixture, "  fixture: base\n  config: { installation: { orgs: x } }\n", wantUnknownField},
		{"unknown team field", atFixture, "  fixture: base\n  config: { teams: [{ name: bumblebee, capp: 1 }] }\n", wantUnknownField},
		{"unknown person field", atFixture, "  fixture: base\n  config: { people: [{ login: alice, wipp: 1 }] }\n", wantUnknownField},
		{"bad clock", atFixture, "  fixture: base\n  clock: monday\n", "clock"},
		{"bad replicas", atFixture, "  fixture: base\n  engine: { replicas: 3 }\n", "replicas"},
		{"unknown engine key", atFixture, "  fixture: base\n  engine: { leaders: 2 }\n", wantUnknownField},
		{"bad history", atFixture, "  fixture: base\n  history: { ci: [{ repo: a/b, durations: [soon] }] }\n", "history.ci"},
		{"two documents", atSteps, "steps:\n  - { id: s, tool: settle }\n---\nname: x\n", "more than one YAML document"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(valid, c.from) {
				t.Fatalf("case doesn't apply: %q not in the valid scenario", c.from)
			}
			_, err := parseValid(t, strings.Replace(valid, c.from, c.to, 1))
			if err == nil {
				t.Fatalf("loaded; want an error containing %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q doesn't contain %q", err, c.want)
			}
		})
	}
}

func TestPreConfigurationMergesOntoFixture(t *testing.T) {
	t.Parallel()
	s, err := parseValid(t, strings.Replace(valid, atFixture, `  fixture: base
  clock: "2026-11-16T20:00:00Z"
  engine: { replicas: 2 }
  config:
    teams: [{ name: bumblebee, cap: { items: 1 } }]
    people: [{ login: carol, teams: [bumblebee], wip: { items: 2, epics: 1 } }]
  github:
    repos: [{ name: alice/klaus-experiments, visibility: public, installed: false }, { name: giantswarm/klaus, requiredApprovals: 2 }]
  history:
    ci: [{ repo: giantswarm/klaus, durations: [20m, 25m, 30m] }]
`, 1))
	if err != nil {
		t.Fatal(err)
	}
	p := s.Pre
	if got := p.Clock.Format("15:04"); got != "20:00" {
		t.Errorf("clock %s, want the scenario's", got)
	}
	if p.Engine.Replicas != 2 {
		t.Errorf("replicas %d", p.Engine.Replicas)
	}
	teams, _ := p.Config.TeamConfigs()
	if len(teams) != 1 || teams[0].BoardOption != "Bumblebee🐝" || teams[0].Cap == nil || *teams[0].Cap.Items != 1 {
		t.Errorf("teams not merged by name: %+v", teams)
	}
	people, _ := p.Config.PersonSpecs()
	if len(people) != 3 || people[2].Login != "carol" || people[0].Login != "alice" {
		t.Errorf("people not merged by login: %+v", people)
	}
	repos := p.GitHub["repos"].([]any)
	if len(repos) != 4 {
		t.Fatalf("repos not merged by name: %d", len(repos))
	}
	klaus := repos[1].(map[string]any)
	if klaus["requiredApprovals"] != 2 || klaus["defaultBranch"] != "main" {
		t.Errorf("giantswarm/klaus not merged: %v", klaus)
	}
	if len(p.History.CI) != 1 || p.History.CI[0].Repo != "giantswarm/klaus" {
		t.Errorf("history %+v", p.History)
	}
	spec, err := p.Config.InstallationSpec()
	if err != nil || spec.Org != "giantswarm" || spec.Waits.CI.DefaultDeadline.Duration.String() != "1h0m0s" {
		t.Errorf("installation %+v %v", spec, err)
	}
}

func TestBudget(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		steps string
		want  string
	}{
		{"", "1s"},
		{"  - { id: a, tool: clock_advance, args: { by: 59m } }\n", "1s"},
		{"  - { id: a, tool: clock_advance, args: { by: 30m } }\n  - { id: b, tool: clock_advance, args: { by: 30m } }\n", "3s"},
		{"  - { id: a, tool: clock_advance, args: { by: 6h } }\n", "3s"},
	} {
		s, err := parseValid(t, valid+c.steps)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Budget().String(); got != c.want {
			t.Errorf("%q: budget %s, want %s", c.steps, got, c.want)
		}
	}
}

// scenarioFiles counts the scenario files of a concept directory (a glob; fixtures aren't scenarios).
func scenarioFiles(t *testing.T, concept string) int {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(scenariosDir, concept, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range files {
		if filepath.Base(filepath.Dir(f)) != "fixtures" {
			n++
		}
	}
	return n
}
