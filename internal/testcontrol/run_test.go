package testcontrol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runTimeout is go test's -timeout in the runs of these tests: the hang
// on the base ends in it.
const runTimeout = 4 * time.Second

// The module of TestControlRunsTheTestsOnTheBase, a pull request that
// fixes a bug in Add with its regression test and a test that passes on
// the base anyway, tests a new function, fixes a hang, adds a test that
// fails everywhere and one that skips, and fixes a bug whose test builds
// only with a tag.
var (
	runBase = map[string]string{
		"go.mod":              "module example.com/ctl\n\ngo 1.24\n",
		"fixed/fixed.go":      "package fixed\n\n// Add adds, with the bug the pull request fixes.\nfunc Add(a, b int) int { return a - b }\n",
		"fixed/fixed_test.go": "package fixed\n\nimport \"testing\"\n\nfunc TestAddZero(t *testing.T) {\n\tif Add(0, 0) != 0 {\n\t\tt.Fatal(Add(0, 0))\n\t}\n}\n",
		"newapi/newapi.go":    "package newapi\n\nfunc One() int { return 1 }\n",
		"hang/hang.go":        "package hang\n\n// Done never closes: the bug.\nfunc Done() <-chan struct{} { return make(chan struct{}) }\n",
		"both/both.go":        "package both\n",
		"skips/skips.go":      "package skips\n",
		"tagged/tagged.go":    "package tagged\n\nfunc Value() int { return 1 }\n",
	}
	runHead = map[string]string{
		"fixed/fixed.go": "package fixed\n\n// Add adds.\nfunc Add(a, b int) int { return a + b }\n",
		"fixed/fixed_test.go": `package fixed

import "testing"

func TestAddZero(t *testing.T) {
	if Add(0, 0) != 0 {
		t.Fatal(Add(0, 0))
	}
}

func TestAddAdds(t *testing.T) {
	if got := Add(2, 3); got != 5 {
		t.Fatalf("Add(2, 3) = %d, want 5", got)
	}
}

func TestAddKeepsOne(t *testing.T) {
	if got := Add(1, 0); got != 1 {
		t.Fatalf("Add(1, 0) = %d, want 1", got)
	}
}
`,
		"newapi/newapi.go":      "package newapi\n\nfunc One() int { return 1 }\n\nfunc Two() int { return 2 }\n",
		"newapi/newapi_test.go": "package newapi\n\nimport \"testing\"\n\nfunc TestTwo(t *testing.T) {\n\tif Two() != 2 {\n\t\tt.Fatal(Two())\n\t}\n}\n",
		"hang/hang.go":          "package hang\n\nvar done = make(chan struct{})\n\nfunc init() { close(done) }\n\n// Done is closed.\nfunc Done() <-chan struct{} { return done }\n",
		"hang/hang_test.go":     "package hang\n\nimport \"testing\"\n\nfunc TestDoneIsClosed(t *testing.T) { <-Done() }\n",
		"both/both_test.go":     "package both\n\nimport \"testing\"\n\nfunc TestFailsEverywhere(t *testing.T) { t.Fatal(\"fails on both\") }\n",
		"skips/skips_test.go":   "package skips\n\nimport \"testing\"\n\nfunc TestSkips(t *testing.T) { t.Skip(\"needs what CI lacks\") }\n",
		"tagged/tagged.go":      "package tagged\n\nfunc Value() int { return 2 }\n",
		"tagged/tagged_test.go": "//go:build integration\n\npackage tagged\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) {\n\tif Value() != 2 {\n\t\tt.Fatal(Value())\n\t}\n}\n",
	}
)

// The control on a real history, with go test: each new or changed test
// runs on the merge base with the pull request's test files applied, the
// ones that fail there again on the pull request's code, and the control
// passes on the tests that fail on the base and pass on the change.
func TestControlRunsTheTestsOnTheBase(t *testing.T) {
	t.Parallel()
	head, base := history(t, runBase, runHead)
	logs := t.TempDir()
	rep, err := Control(t.Context(), Options{Head: head, Base: base, Logs: logs, Timeout: runTimeout})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	if err := rep.Text(&text); err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + text.String())
	type want struct{ base, head, detail string }
	wants := map[string]want{
		"fixed.TestAddAdds":        {Fails, Passes, "Add(2, 3) = -1, want 5"},
		"fixed.TestAddKeepsOne":    {Passes, "", ""},
		"newapi.TestTwo":           {NoBuild, "", "Two"},
		"hang.TestDoneIsClosed":    {Fails, Passes, ""},
		"both.TestFailsEverywhere": {Fails, Fails, "on the pull request's code"},
		"skips.TestSkips":          {Skipped, "", "needs what CI lacks"},
		"tagged.TestValue":         {Fails, Passes, "1"},
	}
	if len(rep.Cases) != len(wants) {
		t.Errorf("%d cases, want %d", len(rep.Cases), len(wants))
	}
	for _, c := range rep.Cases {
		key := c.Dir + "." + c.Name
		w, ok := wants[key]
		if !ok {
			t.Errorf("unexpected case %+v", c)
			continue
		}
		if c.Skip != "" || c.Base != w.base || c.Head != w.head || !strings.Contains(c.Detail, w.detail) {
			t.Errorf("%s: skip %q, %q on the base, %q on the change, detail %q; want %q, %q with %q", key, c.Skip, c.Base, c.Head, c.Detail, w.base, w.head, w.detail)
		}
	}
	if !rep.OK || !strings.Contains(rep.Summary, "3 of 7 test(s)") {
		t.Errorf("ok %t, %q; want the control passed by 3 of 7 tests", rep.OK, rep.Summary)
	}
	if !strings.Contains(text.String(), "OK   fixed TestAddAdds (added): fails on the base, passes on the pull request's code") {
		t.Errorf("the report does not say TestAddAdds shows the change:\n%s", text.String())
	}
	if _, err := os.Stat(filepath.Join(base, "newapi", "newapi_test.go")); err != nil {
		t.Errorf("the pull request's test files are not on the base: %v", err)
	}
	if cmd, err := os.ReadFile(filepath.Join(logs, "base-1.cmd")); err != nil || !strings.Contains(string(cmd), "go test -json") { //nolint:gosec // the test's own logs
		t.Errorf("the first run's command: %q, %v", cmd, err)
	}
	dir := t.TempDir()
	if err := rep.Write(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "report.json")) //nolint:gosec // the test's own report
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(raw, &back); err != nil || back.OK != rep.OK || len(back.Cases) != len(rep.Cases) {
		t.Errorf("report.json: %v, %+v", err, back)
	}
}

// A fix whose new tests pass on the base or do not build there shows
// nothing: the control fails, names both and says what to do.
func TestControlFailsWithoutATestThatShowsTheChange(t *testing.T) {
	t.Parallel()
	head := map[string]string{
		"fixed/fixed.go": runHead["fixed/fixed.go"],
		"fixed/fixed_test.go": runBase["fixed/fixed_test.go"] +
			"\nfunc TestAddKeepsOne(t *testing.T) {\n\tif Add(1, 0) != 1 {\n\t\tt.Fatal(Add(1, 0))\n\t}\n}\n",
		"newapi/newapi.go":      runHead["newapi/newapi.go"],
		"newapi/newapi_test.go": runHead["newapi/newapi_test.go"],
	}
	h, base := history(t, runBase, head)
	rep, err := Control(t.Context(), Options{Head: h, Base: base, Timeout: runTimeout})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	if err := rep.Text(&text); err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + text.String())
	if rep.OK || !strings.Contains(rep.Summary, "none of the 2 test(s)") || !strings.Contains(rep.Summary, "1 of them do not build there") {
		t.Fatalf("ok %t, %q; want a failed control naming the test that does not build", rep.OK, rep.Summary)
	}
	if !strings.Contains(text.String(), "fixed TestAddKeepsOne (added): passes on the base") {
		t.Errorf("the report does not name the test that passes on the base:\n%s", text.String())
	}
}
