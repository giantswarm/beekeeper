//nolint:goconst // Literal go test -json events and fixture paths keep the external protocol visible.
package testcontrol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// gone, as a file's content in a tree, deletes the file.
const gone = "\x00gone"

// writeTree writes files, by slash path, under root.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		f := filepath.Join(root, filepath.FromSlash(p))
		if content == gone {
			if err := os.Remove(f); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(f), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// names are the names of tests.
func names(tests []*Test) []string {
	var out []string
	for _, t := range tests {
		out = append(out, t.Name)
	}
	return out
}

// Only the functions go test runs as tests are tests.
func TestTestsOfFindsTheTestFunctions(t *testing.T) {
	src := `package x

import (
	"testing"
	tt "testing"
)

type s struct{}

func Test(t *testing.T)                    {}
func TestA(t *testing.T)                   {}
func Test_b(t *tt.T)                       {}
func TestÄ(*testing.T)                     {}
func Testlower(t *testing.T)               {}
func TestMain(m *testing.M)                {}
func TestResult(t *testing.T) error        { return nil }
func TestTwo(t *testing.T, n int)          {}
func TestGeneric[P any](t *testing.T)      {}
func (s) TestMethod(t *testing.T)          {}
func BenchmarkB(b *testing.B)              {}
func FuzzF(f *testing.F)                   {}
func helper(t *testing.T)                  {}
`
	tests, err := testsOf("pkg/x_test.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(tests), []string{"Test", "TestA", "Test_b", "TestÄ"}; !slices.Equal(got, want) {
		t.Fatalf("tests %q, want %q", got, want)
	}
	if tests[0].Dir != "pkg" || tests[0].File != "pkg/x_test.go" {
		t.Fatalf("test %+v, want its file and its package's directory", tests[0])
	}
}

// A test whose doc comment, comments or blank lines change is the same
// test; one whose code changes is not.
func TestCanonicalIgnoresCommentsAndBlankLines(t *testing.T) {
	code := func(src string) string {
		t.Helper()
		tests, err := testsOf("x_test.go", []byte("package x\n\nimport \"testing\"\n\n"+src))
		if err != nil {
			t.Fatal(err)
		}
		if len(tests) != 1 {
			t.Fatalf("%d tests in %q", len(tests), src)
		}
		return tests[0].Code
	}
	base := code("func TestX(t *testing.T) {\n\tn := 1\n\tif n != 1 {\n\t\tt.Fatal(n)\n\t}\n}\n")
	same := code("// TestX checks n.\n//\n// control: none: a reason that does not change the code\nfunc TestX(t *testing.T) {\n\t// one\n\tn := 1\n\n\n\tif n != 1 { // never\n\t\tt.Fatal(n)\n\t}\n}\n")
	if same != base {
		t.Fatalf("a change of comments and blank lines changed the code:\n%s\n---\n%s", base, same)
	}
	if other := code("func TestX(t *testing.T) {\n\tn := 2\n\tif n != 1 {\n\t\tt.Fatal(n)\n\t}\n}\n"); other == base {
		t.Fatal("a change of the code went unseen")
	}
}

// A control paragraph may wrap; its reason needs words.
func TestControlLines(t *testing.T) {
	tests, err := testsOf("x_test.go", []byte(`package x

import "testing"

// TestWrapped checks a new flag.
//
// control: none: the flag is new, so the base cannot
// parse it at all
//
// More prose.
func TestWrapped(t *testing.T) {}

// control: none: too short
func TestShort(t *testing.T) {}

// control: maybe later, when the base has it
func TestNotNone(t *testing.T) {}

// Without one.
func TestPlain(t *testing.T) {}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := tests[0].Control; got != "control: none: the flag is new, so the base cannot parse it at all" {
		t.Fatalf("control line %q, want the paragraph on one line", got)
	}
	if reason, err := optOut(tests[0].Control); err != nil || reason != "the flag is new, so the base cannot parse it at all" {
		t.Fatalf("reason %q, %v", reason, err)
	}
	for _, tc := range tests[1:3] {
		if _, err := optOut(tc.Control); err == nil || !strings.Contains(err.Error(), "control: none: <why") {
			t.Errorf("%s: %q read as an opt-out (%v)", tc.Name, tc.Control, err)
		}
	}
	if tests[3].Control != "" {
		t.Errorf("TestPlain has the control line %q", tests[3].Control)
	}
}

// A changed file is a test file, test data, or anything else that a test
// may exercise.
func TestClassify(t *testing.T) {
	for p, want := range map[string]int{
		"internal/x/x.go":               kindCode,
		"internal/x/x_test.go":          kindTest,
		"internal/x/testdata/a.json":    kindTestData,
		"internal/x/testdata/y_test.go": kindTestData,
		"internal/x/testdata/z.go":      kindTestData,
		".circleci/cosign-retry.sh":     kindCode,
		"docs/scenarios.md":             kindCode,
		"go.mod":                        kindCode,
	} {
		if got := classify(p); got != want {
			t.Errorf("%s: kind %d, want %d", p, got, want)
		}
	}
}

// The fewest tags CI tests with that build the file; none for a live
// test.
func TestTagsFor(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"p/plain_test.go":    "package p\n",
		"p/scenario_test.go": "//go:build scenario\n\npackage p\n",
		"p/not_test.go":      "//go:build !scenario\n\npackage p\n",
		"p/both_test.go":     "//go:build perf && integration\n\npackage p\n",
		"p/live_test.go":     "//go:build hyprlive\n\npackage p\n",
	})
	for file, want := range map[string][]string{
		"p/plain_test.go":    nil,
		"p/scenario_test.go": {"scenario"},
		"p/not_test.go":      nil,
		"p/both_test.go":     {"integration", "perf"},
	} {
		got, ok := tagsFor(root, file)
		if !ok || !slices.Equal(got, want) {
			t.Errorf("%s: tags %q, %t; want %q", file, got, ok, want)
		}
	}
	if got, ok := tagsFor(root, "p/live_test.go"); ok {
		t.Errorf("a live test runs with %q", got)
	}
	if got := subsets([]string{"a", "b"}); len(got) != 4 || len(got[0]) != 0 || !slices.Equal(got[3], []string{"a", "b"}) {
		t.Errorf("subsets %q", got)
	}
}

// go test -json's events, read into each test's run: a failure, a pass,
// a skip, a package that does not build, a binary that times out during
// a test, a failed setup, a TestMain that fails and a test that does not
// run.
func TestParseEvents(t *testing.T) {
	var lines []string
	add := func(es ...event) {
		for _, e := range es {
			raw, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			lines = append(lines, string(raw))
		}
	}
	const a, b, c, d, e, f = "m/a", "m/b", "m/c", "m/d", "m/e", "m/f"
	add(
		event{Action: "run", Package: a, Test: "TestF"},
		event{Action: "output", Package: a, Test: "TestF", Output: "=== RUN   TestF\n"},
		event{Action: "run", Package: a, Test: "TestF/sub"},
		event{Action: "output", Package: a, Test: "TestF/sub", Output: "    a_test.go:9: boom in sub\n"},
		event{Action: "fail", Package: a, Test: "TestF/sub"},
		event{Action: "output", Package: a, Test: "TestF", Output: "--- FAIL: TestF (0.00s)\n"},
		event{Action: "fail", Package: a, Test: "TestF"},
		event{Action: "run", Package: a, Test: "TestP"},
		event{Action: "pass", Package: a, Test: "TestP"},
		event{Action: "run", Package: a, Test: "TestS"},
		event{Action: "output", Package: a, Test: "TestS", Output: "    a_test.go:20: not here\n"},
		event{Action: "skip", Package: a, Test: "TestS"},
		event{Action: "fail", Package: a},
		event{Action: "build-output", ImportPath: b + " [m/b.test]", Output: "# m/b [m/b.test]\n"},
		event{Action: "build-output", ImportPath: b + " [m/b.test]", Output: "b/b_test.go:3:2: undefined: Two\n"},
		event{Action: "build-fail", ImportPath: b + " [m/b.test]"},
		event{Action: "output", Package: b, Output: "FAIL\tm/b [build failed]\n"},
		event{Action: "fail", Package: b, FailedBuild: b + " [m/b.test]"},
		event{Action: "run", Package: c, Test: "TestH"},
		event{Action: "output", Package: c, Output: "panic: test timed out after 4s\n"},
		event{Action: "fail", Package: c},
		event{Action: "output", Package: d, Output: "FAIL\tm/d [setup failed]\n"},
		event{Action: "fail", Package: d},
		event{Action: "output", Package: e, Output: "TestMain: no database\n"},
		event{Action: "fail", Package: e},
		event{Action: "output", Package: f, Output: "testing: warning: no tests to run\n"},
		event{Action: "pass", Package: f},
	)
	lines = append(lines, "go: a line that is not an event")
	res, err := parseEvents(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ pkg, test, verdict, detail string }{
		{a, "TestF", Fails, "boom in sub"},
		{a, "TestP", Passes, ""},
		{a, "TestS", Skipped, "not here"},
		{b, "TestT", NoBuild, "undefined: Two"},
		{c, "TestH", Fails, "timed out"},
		{d, "TestD", NoBuild, "setup failed"},
		{e, "TestE", Crashed, "no database"},
		{f, "TestN", NotRun, "no tests to run"},
	} {
		v, detail := res.verdict(tc.pkg, tc.test)
		if v != tc.verdict || !strings.Contains(detail, tc.detail) {
			t.Errorf("%s %s: %q, %q; want %q with %q", tc.pkg, tc.test, v, detail, tc.verdict, tc.detail)
		}
	}
	if v, detail := res.verdict(a, "TestF"); strings.Contains(detail, "=== RUN") {
		t.Errorf("%s detail keeps the run lines: %q", v, detail)
	}
	// A package the go command did not get to: its error is on stderr.
	if v, detail := res.verdict("m/g", "TestG"); v != NotRun || !strings.Contains(detail, "not an event") {
		t.Errorf("m/g before the go command failed: %q, %q", v, detail)
	}
	res.failed = true
	if v, _ := res.verdict("m/g", "TestG"); v != NoBuild {
		t.Errorf("m/g after the go command failed: %q, want %q", v, NoBuild)
	}
}

// The control passes when a test shows the change or none runs, and fails
// with what to do when none shows it.
func TestJudge(t *testing.T) {
	shows := &Case{Name: "TestShows", Base: Fails, Head: Passes}
	passes := &Case{Name: "TestPasses", Base: Passes}
	noBuild := &Case{Name: "TestNoBuild", Base: NoBuild}
	everywhere := &Case{Name: "TestEverywhere", Base: Fails, Head: Fails}
	skipped := &Case{Name: "TestSkipped", Skip: SkipOptedOut, Reason: "covers what main already does"}
	for _, tc := range []struct {
		name     string
		cases    []*Case
		problems []string
		ok       bool
		summary  string
	}{
		{"no test", nil, nil, true, "adds or changes no Test function"},
		{"all skipped", []*Case{skipped}, nil, true, "none of the 1 new or changed test(s) runs"},
		{"one shows it", []*Case{passes, noBuild, shows, skipped}, nil, true, "1 of 3 test(s)"},
		{"none shows it", []*Case{passes, noBuild, everywhere, skipped}, nil, false, "1 of them do not build there"},
		{"a problem", []*Case{shows}, []string{"x_test.go: TestX: bad"}, false, "cannot be read"},
	} {
		rep := &Report{Cases: tc.cases, Problems: tc.problems}
		rep.judge()
		if rep.OK != tc.ok || !strings.Contains(rep.Summary, tc.summary) {
			t.Errorf("%s: ok %t, %q; want %t with %q", tc.name, rep.OK, rep.Summary, tc.ok, tc.summary)
		}
		if !rep.OK && len(tc.problems) == 0 && !strings.Contains(rep.Summary, "control: none: <why>") {
			t.Errorf("%s: the failure does not say how to opt out: %q", tc.name, rep.Summary)
		}
	}
}
