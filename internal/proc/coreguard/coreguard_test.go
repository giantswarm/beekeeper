// Package coreguard holds the guard that keeps beekeeper's core asking
// internal/proc, not the operating system, for what the process seam covers
// (docs/plans/3788-behavioral-scenarios.md). It is a package of tests only,
// so that it runs on any checkout, a merge base included.
package coreguard

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// core are the directories of beekeeper's core, from the repository's root.
var core = []string{"cmd", "internal", "pkg"}

// harnesses are the test harnesses under internal/: they start the real
// binary, its fakes and Go itself on purpose, and none of their code runs in
// a desk's processes. internal/proc is the seam itself. internal/loopworkflow
// is core, not a harness: self-update's canary and the candidate and
// recovery probes run it on a desk, and the release binary serves its
// fixtures.
var harnesses = []string{
	"internal/proc",
	"internal/scenario",
	"internal/testenv",
	"internal/perfsuite",
	"internal/testcontrol",
}

// forbidden are the imports the core reaches through internal/proc, each
// with what replaces it. A seam that moves more of the operating system
// behind internal/proc adds its imports here.
var forbidden = map[string]string{
	"os/exec": "internal/proc: CommandContext, Command, Cmd, ExitError, LookPath",
}

// TestCoreUsesProc fails when a Go file of the core (not its tests) imports
// what internal/proc stands in for: a process such a file starts would
// bypass the seam that answers it in a behavioral scenario.
func TestCoreUsesProc(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	var findings []string
	for _, dir := range core {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if slices.Contains(harnesses, rel) || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			found, err := forbiddenImports(path, nil)
			for _, f := range found {
				findings = append(findings, rel+":"+f)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(findings) > 0 {
		t.Fatalf("%d imports in the core of what internal/proc stands in for:\n%s",
			len(findings), strings.Join(findings, "\n"))
	}
}

// TestGuardFindsForbiddenImports is the guard's negative control: a plain
// and a renamed import are found, a blank one too, other imports are not.
func TestGuardFindsForbiddenImports(t *testing.T) {
	src := `package p

import (
	"os"
	run "os/exec"
)

import _ "os/exec"

var _ = run.Command
var _ = os.Args
`
	got, err := forbiddenImports("p.go", src)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`5: "os/exec" (use internal/proc: CommandContext, Command, Cmd, ExitError, LookPath)`,
		`8: "os/exec" (use internal/proc: CommandContext, Command, Cmd, ExitError, LookPath)`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("found %q, want %q", got, want)
	}
}

// forbiddenImports lists the forbidden imports of the Go file at path (src
// when set), each as `line: "path" (use what)`.
func forbiddenImports(path string, src any) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		if use, ok := forbidden[p]; ok {
			out = append(out, fmt.Sprintf("%d: %q (use %s)", fset.Position(imp.Pos()).Line, p, use))
		}
	}
	return out, nil
}
