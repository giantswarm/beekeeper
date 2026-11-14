package harness

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func y(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := yaml.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMerge(t *testing.T) {
	t.Parallel()
	const listOfA = "{l: [{name: a}]}" // a list of one object named a
	cases := []struct {
		name, base, over, want string
	}{
		{"scalar overrides", "{a: 1, b: x}", "{b: y}", "{a: 1, b: y}"},
		{"maps merge deeply", "{a: {b: 1, c: 2}}", "{a: {c: 3, d: 4}}", "{a: {b: 1, c: 3, d: 4}}"},
		{"new key added", "{a: 1}", "{z: [1]}", "{a: 1, z: [1]}"},
		{"null overrides", "{a: {b: 1}}", "{a: null}", "{a: null}"},
		{"scalar lists replace", "{l: [1, 2, 3]}", "{l: [4]}", "{l: [4]}"},
		{"empty list replaces", listOfA, "{l: []}", "{l: []}"},
		{"lists merge by name", "{l: [{name: a, x: 1}, {name: b, x: 2}]}", "{l: [{name: b, y: 3}, {name: c}]}", "{l: [{name: a, x: 1}, {name: b, x: 2, y: 3}, {name: c}]}"},
		{"lists merge by ref", "{l: [{ref: o/r#1, t: a}]}", "{l: [{ref: o/r#1, t: b}, {ref: o/r#2}]}", "{l: [{ref: o/r#1, t: b}, {ref: o/r#2}]}"},
		{"lists merge by login", "{p: [{login: alice, wip: {items: 1, epics: 1}}]}", "{p: [{login: alice, wip: {items: 2}}]}", "{p: [{login: alice, wip: {items: 2, epics: 1}}]}"},
		{"nested named lists merge", "{l: [{ref: x, checks: [{name: a, s: 1}]}]}", "{l: [{ref: x, checks: [{name: a, s: 2}, {name: b}]}]}", "{l: [{ref: x, checks: [{name: a, s: 2}, {name: b}]}]}"},
		{"unnamed lists replace", "{l: [{point: p, item: a}]}", "{l: [{point: q, item: b}]}", "{l: [{point: q, item: b}]}"},
		{"mixed lists replace", "{l: [{name: a}, 1]}", listOfA, listOfA},
		{"over onto nothing", "{}", listOfA, listOfA},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			base, over := y(t, c.base), y(t, c.over)
			baseCopy := clone(base)
			got := Merge(base, over)
			if want := y(t, c.want); !reflect.DeepEqual(got, want) {
				t.Errorf("Merge(%s, %s) = %s, want %s", c.base, c.over, render(got), render(want))
			}
			if !reflect.DeepEqual(base, baseCopy) {
				t.Errorf("Merge modified its base")
			}
		})
	}
}

func TestMergeConfig(t *testing.T) {
	t.Parallel()
	s, err := parseValid(t, valid)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := MergeConfig(s.Pre.Config, y(t, `{teams: [{name: bumblebee, cap: {items: 2}}, {name: atlas, boardOption: "Atlas 🗺️"}], people: [{login: bob, wip: {items: 3}}], installation: {resync: {board: 5m}}}`).(map[string]any))
	if err != nil {
		t.Fatal(err)
	}
	teams, _ := cfg.TeamConfigs()
	if len(teams) != 2 || *teams[0].Cap.Items != 2 || teams[0].BoardOption != "Bumblebee🐝" || teams[1].Name != "atlas" {
		t.Errorf("teams %+v", teams)
	}
	people, _ := cfg.PersonSpecs()
	if people[1].WIP.Items != 3 || people[1].WIP.Epics != 1 || len(people[1].Teams) != 1 {
		t.Errorf("bob %+v", people[1])
	}
	spec, _ := cfg.InstallationSpec()
	if spec.Resync.Board.Duration.String() != "5m0s" || spec.Resync.Changes.Duration.String() != "15m0s" {
		t.Errorf("installation %+v", spec.Resync)
	}
	// The original is untouched.
	orig, _ := s.Pre.Config.TeamConfigs()
	if orig[0].Cap != nil {
		t.Errorf("MergeConfig modified its base")
	}
	if _, err := MergeConfig(s.Pre.Config, y(t, `{teams: [{name: bumblebee, capp: 1}]}`).(map[string]any)); err == nil {
		t.Errorf("an unknown team field was accepted")
	}
}
