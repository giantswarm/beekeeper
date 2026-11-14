package harness

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// result is what the evaluator cases run against.
const result = `
count: 2
value: 0.9
ok: true
empty: ""
none: null
list: []
rows:
  - { type: wait.woken, source: webhook, at: "2026-11-16T09:05:00Z", data: { on: ci } }
  - { type: wait.ended, source: resync, at: "2026-11-16T09:10:00Z", data: { on: ci, reason: Bumblebee🐝 } }
status:
  phase: Ready
  changes: [giantswarm/klaus#55]
  wait: { on: ci, ref: "ci:giantswarm/klaus@a1", backoff: 2m0s }
  conditions:
    - { type: BoardFilterValid, status: "False", message: 'unknown team option "Bumblebee 🐝"' }
  shadow:
    actions:
      - { key: a1, type: assign, args: { login: alice } }
      - { key: a2, type: set-status, args: { status: "In Progress ⛏️" } }
`

func expectation(t *testing.T, s string) *Expected {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(s))
	dec.KnownFields(true)
	var e Expected
	if err := dec.Decode(&e); err != nil {
		t.Fatal(err)
	}
	if p := e.check(); len(p) > 0 {
		t.Fatalf("expectation %q doesn't load: %v", s, p)
	}
	return &e
}

func TestEvaluate(t *testing.T) {
	t.Parallel()
	doc := y(t, result)
	cases := []struct {
		name, expected string
		err            error
		pass           bool
	}{
		{"nothing expected", "{}", nil, true},
		{"success by default", "{}", errors.New("boom"), false},
		{"success true fails on error", "{success: true}", errors.New("boom"), false},
		{"success false fails on success", "{success: false}", nil, false},
		{"success false passes on error", "{success: false}", errors.New("item not found"), true},
		{"error_contains, case-insensitive", "{success: false, error_contains: [NOT FOUND]}", errors.New("item x: not found"), true},
		{"error_contains missing", "{success: false, error_contains: [forbidden]}", errors.New("item x: not found"), false},
		{"error_contains on empty error", "{success: false, error_contains: [x]}", errors.New(""), false},

		{"json_path scalar", "{json_path: {status.phase: Ready}}", nil, true},
		{"json_path scalar wrong", "{json_path: {status.phase: Done}}", nil, false},
		{"json_path number", "{json_path: {count: 2}}", nil, true},
		{"json_path float", "{json_path: {value: 0.9}}", nil, true},
		{"json_path number wrong", "{json_path: {count: 3}}", nil, false},
		{"json_path bool", "{json_path: {ok: true}}", nil, true},
		{"json_path bool as string", `{json_path: {ok: "true"}}`, nil, true},
		{"json_path string False", `{json_path: {"status.conditions[type=BoardFilterValid].status": "False"}}`, nil, true},
		{"json_path index", "{json_path: {rows.1.type: wait.ended}}", nil, true},
		{"json_path bracket index", "{json_path: {\"rows[1].source\": resync}}", nil, true},
		{"json_path index out of range", "{json_path: {rows.2.type: wait.ended}}", nil, false},
		{"json_path selector", "{json_path: {\"status.shadow.actions[type=assign].args.login\": alice}}", nil, true},
		{"json_path selector wrong", "{json_path: {\"status.shadow.actions[type=assign].args.login\": bob}}", nil, false},
		{"json_path selector no match", "{json_path: {\"status.shadow.actions[type=todo].args.to\": alice}}", nil, false},
		{"json_path selector absent is null", "{json_path: {\"status.shadow.actions[type=todo]\": null}}", nil, true},
		{"json_path partial map", "{json_path: {status.wait: {on: ci}}}", nil, true},
		{"json_path partial map wrong", "{json_path: {status.wait: {on: review}}}", nil, false},
		{"json_path nested map with path keys", "{json_path: {status: {wait.on: ci, phase: Ready}}}", nil, true},
		{"json_path map against scalar", "{json_path: {status.phase: {x: 1}}}", nil, false},
		{"json_path exact array", "{json_path: {status.changes: [giantswarm/klaus#55]}}", nil, true},
		{"json_path array too long", "{json_path: {status.changes: [giantswarm/klaus#55, giantswarm/klaus#56]}}", nil, false},
		{"json_path array elements partial", "{json_path: {rows: [{type: wait.woken}, {data: {on: ci}}]}}", nil, true},
		{"json_path array element wrong", "{json_path: {rows: [{type: wait.woken}, {type: wait.woken}]}}", nil, false},
		{"json_path array length checked", "{json_path: {rows: [{type: wait.woken}]}}", nil, false},
		{"json_path empty array", "{json_path: {list: []}}", nil, true},
		{"json_path null absent", "{json_path: {status.refusal: null}}", nil, true},
		{"json_path null empty string", "{json_path: {empty: null}}", nil, true},
		{"json_path null empty list", "{json_path: {list: null}}", nil, true},
		{"json_path null null", "{json_path: {none: null}}", nil, true},
		{"json_path null present", "{json_path: {status.phase: null}}", nil, false},
		{"json_path missing", "{json_path: {status.refusal.code: 404}}", nil, false},
		{"json_path time", `{json_path: {rows.0.at: "2026-11-16T10:05:00+01:00"}}`, nil, true},
		{"json_path time wrong", `{json_path: {rows.0.at: "2026-11-16T09:06:00Z"}}`, nil, false},
		{"json_path duration", "{json_path: {status.wait.backoff: 2m}}", nil, true},
		{"json_path duration wrong", "{json_path: {status.wait.backoff: 1m}}", nil, false},
		{"json_path path into a string", "{json_path: {status.phase.x: 1}}", nil, false},

		{"json_path_max holds", "{json_path_max: {count: 2}}", nil, true},
		{"json_path_max exceeded", "{json_path_max: {count: 1}}", nil, false},
		{"json_path_max missing", "{json_path_max: {nope: 1}}", nil, false},
		{"json_path_max not a number", "{json_path_max: {status.phase: 1}}", nil, false},
		{"json_path_min holds", "{json_path_min: {count: 2, value: 0.5}}", nil, true},
		{"json_path_min short", "{json_path_min: {count: 3}}", nil, false},

		{"contains one missing", "{contains: [WAIT.WOKEN, inputsHash, ci:giantswarm/klaus@a1]}", nil, false},
		{"contains another missing", "{contains: [WAIT.WOKEN, \"bumblebee 🐝\", \"<none>\"]}", nil, false},
		{"contains all present", "{contains: [WAIT.WOKEN, \"Bumblebee 🐝\", \"in progress ⛏️\"]}", nil, true},
		{"not_contains holds", "{not_contains: [bob, gs-plan]}", nil, true},
		{"not_contains found", "{not_contains: [ALICE]}", nil, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := expectation(t, c.expected)
			res := doc
			if c.err != nil {
				res = nil
			}
			fails := e.Evaluate(res, c.err)
			if c.pass && len(fails) > 0 {
				t.Errorf("%s: want pass, got %v", c.expected, fails)
			}
			if !c.pass && len(fails) == 0 {
				t.Errorf("%s: want a failure, it passed", c.expected)
			}
		})
	}
}

func TestEvaluateNilExpectation(t *testing.T) {
	t.Parallel()
	var e *Expected
	if f := e.Evaluate(map[string]any{"a": 1}, nil); len(f) > 0 {
		t.Errorf("nil expectation failed a success: %v", f)
	}
	if f := e.Evaluate(nil, errors.New("boom")); len(f) == 0 {
		t.Errorf("nil expectation passed an error")
	}
}

func TestEvaluateTypedResults(t *testing.T) {
	t.Parallel()
	type row struct {
		Count int     `json:"count"`
		Value float64 `json:"value"`
	}
	e := expectation(t, "{json_path: {count: 3}, json_path_min: {value: 0.5}}")
	if f := e.Evaluate(row{Count: 3, Value: 0.5}, nil); len(f) > 0 {
		t.Errorf("typed result: %v", f)
	}
}

func TestParsePath(t *testing.T) {
	t.Parallel()
	for p, want := range map[string]string{
		"a.b.0.c":                     "a b 0 c",
		"status.conditions[type=X].s": "status conditions [type=X] s",
		"rows[0].data":                "rows 0 data",
		"a[k=v.w]":                    "a [k=v.w]",
		"checks[name=ci/circleci: go-test].status": "checks [name=ci/circleci: go-test] status",
	} {
		segs, err := parsePath(p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		var parts []string
		for _, s := range segs {
			parts = append(parts, s.String())
		}
		if got := strings.Join(parts, " "); got != want {
			t.Errorf("%s: %q, want %q", p, got, want)
		}
	}
	for _, bad := range []string{"", ".a", "a.", "a..b", "a[", "a[=x]", "a[x]", "a[0]b"} {
		if _, err := parsePath(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
