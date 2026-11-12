package clock_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// migrated are the core flows that take their time from this package: a
// package directory (its own files, not its subpackages), or in cmd, which
// holds every command in one package, the files of a flow by pattern, so
// that a new file of a flow is checked as well. Paths are from the
// repository's root. A flow migrated later is added here.
var migrated = []string{
	// The supervisor loop, its planner and the timers it fires.
	"cmd/supervisor*.go", "cmd/loop*.go", "cmd/changepause.go", "cmd/root.go", "cmd/throughputhealth.go",
	"cmd/timer*.go", "internal/loop", "internal/superloop",
	// Capacity.
	"cmd/capacity*.go", "cmd/usage.go", "cmd/claude_usage.go", "internal/capacity",
	// The GitHub budget.
	"cmd/budget*.go", "cmd/backgroundrest.go", "cmd/githubcalls.go", "internal/github",
	// The gate.
	"cmd/gate*.go", "cmd/ungated.go", "internal/merge",
	// Self-update and probation.
	"cmd/selfupdate*.go", "cmd/recovery.go", "internal/update",
	// The state every flow records in.
	"internal/state",
	// The watch's loops and samples: the supervisor loop owns one, whose
	// budget reading admits the loop's starts.
	"cmd/watch.go",
	// board next: its pick weighs the delivery limit against the loop's
	// delivery observation, dated on the world's clock.
	"cmd/board.go",
}

// clockFuncs are the time package's clock functions, each with what
// replaces it.
var clockFuncs = map[string]string{
	"Now":       "clock.Now",
	"Since":     "clock.Since",
	"Until":     "clock.Until",
	"After":     "clock.After",
	"Sleep":     "clock.Sleep",
	"NewTimer":  "clock.NewTimer",
	"NewTicker": "clock.NewTicker",
	"Tick":      "clock.NewTicker",
	"AfterFunc": "clock.AfterFunc",
}

// TestMigratedPackagesUseTheClock fails when a file of a migrated flow
// (not its tests) uses the time package's clock instead of this package:
// in a scenario that file would not see the world's time.
func TestMigratedPackagesUseTheClock(t *testing.T) {
	root := filepath.Join("..", "..")
	files := map[string]bool{}
	for _, pattern := range migrated {
		glob := pattern
		if !strings.HasSuffix(pattern, ".go") {
			glob = filepath.Join(pattern, "*.go")
		}
		matches, err := filepath.Glob(filepath.Join(root, glob))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, m := range matches {
			if !strings.HasSuffix(m, "_test.go") {
				files[m] = true
				n++
			}
		}
		if n == 0 {
			t.Errorf("migrated %q matches no Go file: the list is stale", pattern)
		}
	}
	var findings []string
	for _, f := range slices.Sorted(maps.Keys(files)) {
		found, err := systemClockUses(f, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range found {
			rel, _ := filepath.Rel(root, f)
			findings = append(findings, filepath.ToSlash(rel)+":"+line)
		}
	}
	if len(findings) > 0 {
		t.Fatalf("%d uses of the time package's clock in migrated flows; use internal/clock, which a scenario's world advances:\n%s",
			len(findings), strings.Join(findings, "\n"))
	}
}

// TestGuardFindsSystemClockUses is the guard's negative control: calls,
// function values and a renamed import are found, the clock's are not.
func TestGuardFindsSystemClockUses(t *testing.T) {
	src := `package p

import (
	stdtime "time"

	"github.com/giantswarm/beekeeper/internal/clock"
)

var now = stdtime.Now

func f() {
	_ = clock.Now()
	stdtime.Sleep(stdtime.Second)
	t := stdtime.NewTicker(stdtime.Minute)
	defer t.Stop()
	_ = stdtime.Duration(0)
}
`
	got, err := systemClockUses("p.go", src)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"9: stdtime.Now (use clock.Now)",
		"13: stdtime.Sleep (use clock.Sleep)",
		"14: stdtime.NewTicker (use clock.NewTicker)",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("found %q, want %q", got, want)
	}
	if got, err := systemClockUses("q.go", "package q\n\nimport . \"time\"\n\nvar d = Second\n"); err != nil || len(got) != 1 || !strings.Contains(got[0], "dot import") {
		t.Fatalf("a dot import of time: %q %v", got, err)
	}
}

// systemClockUses lists the uses of the time package's clock functions in
// the Go file at path (src when set), each as "line: what (use what)".
func systemClockUses(path string, src any) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []string
	names := map[string]bool{}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != "time" {
			continue
		}
		name := "time"
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch name {
		case "_":
		case ".":
			out = append(out, fmt.Sprintf("%d: a dot import of time hides its clock functions", fset.Position(imp.Pos()).Line))
		default:
			names[name] = true
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || !names[id.Name] {
			return true
		}
		if use, ok := clockFuncs[sel.Sel.Name]; ok {
			out = append(out, fmt.Sprintf("%d: %s.%s (use %s)", fset.Position(sel.Pos()).Line, id.Name, sel.Sel.Name, use))
		}
		return true
	})
	return out, nil
}
