// Package testcontrol is the control of a pull request's Go tests, a step
// of the scenario control (.circleci/scenario-control.sh,
// docs/scenarios.md): the Test functions the pull request adds or changes
// in any _test.go run against its merge base's code with its test files
// applied, and at least one of them must fail there and pass on the pull
// request's own code. A fix whose regression test passes without the fix
// shows nothing; only the pull request's reasoning would support it.
package testcontrol

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// minWords is the fewest words of an opt-out's reason, as for scenarios.
const minWords = 3

// controlPrefix starts a doc comment's control line.
const controlPrefix = "control:"

// Test is a top-level Test function of a _test.go file.
type Test struct {
	// Name is the function's name; File is its file and Dir its package's
	// directory, slash paths relative to the tree's root ("." for the
	// root).
	Name, File, Dir string
	// Code is the declaration without its doc comment, its comments and
	// its blank lines: two tests with the same Code test the same.
	Code string
	// Control is the doc comment's paragraph that starts with "control:",
	// on one line; "" without one.
	Control string
}

// testsOf returns the Test functions of the Go file src, named rel: the
// functions go test runs as tests, func TestXxx(t *testing.T).
func testsOf(rel string, src []byte) ([]*Test, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []*Test
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !isTestName(fn.Name.Name) || !takesT(fn.Type) {
			continue
		}
		code, err := canonical(fset, fn)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", rel, fn.Name.Name, err)
		}
		out = append(out, &Test{Name: fn.Name.Name, File: rel, Dir: path.Dir(rel), Code: code, Control: controlLine(fn.Doc)})
	}
	return out, nil
}

// isTestName is go test's rule for a test's name: Test, then nothing or a
// character that is not a lower-case letter.
func isTestName(name string) bool {
	rest, ok := strings.CutPrefix(name, "Test")
	if !ok {
		return false
	}
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return !unicode.IsLower(r)
}

// takesT says whether a function's signature is a test's: one *testing.T
// parameter, no results and no type parameters.
func takesT(ft *ast.FuncType) bool {
	if ft.TypeParams != nil || ft.Results != nil && len(ft.Results.List) > 0 || ft.Params == nil || len(ft.Params.List) != 1 {
		return false
	}
	p := ft.Params.List[0]
	if len(p.Names) > 1 {
		return false
	}
	star, ok := p.Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	switch x := star.X.(type) {
	case *ast.SelectorExpr:
		return x.Sel.Name == "T"
	case *ast.Ident:
		return x.Name == "T"
	}
	return false
}

// canonical prints a test's declaration without its doc comment: the
// printer leaves out comments of a node that is not a file, and the blank
// lines go, so a change to comments or blank lines is no change.
func canonical(fset *token.FileSet, fn *ast.FuncDecl) (string, error) {
	decl := *fn
	decl.Doc = nil
	var b bytes.Buffer
	if err := (&printer.Config{Mode: printer.TabIndent, Tabwidth: 8}).Fprint(&b, fset, &decl); err != nil {
		return "", err
	}
	var lines []string
	for l := range strings.SplitSeq(b.String(), "\n") {
		if l = strings.TrimRight(l, " \t"); strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n"), nil
}

// controlLine is the paragraph of a doc comment that starts with
// "control:", its lines joined: a reason may wrap.
func controlLine(doc *ast.CommentGroup) string {
	if doc == nil {
		return ""
	}
	var para []string
	for l := range strings.SplitSeq(doc.Text(), "\n") {
		l = strings.TrimSpace(l)
		switch {
		case para == nil && strings.HasPrefix(l, controlPrefix):
			para = []string{l}
		case para != nil && l == "":
			return strings.Join(para, " ")
		case para != nil:
			para = append(para, l)
		}
	}
	return strings.Join(para, " ")
}

// optOut reads a control line, "control: none: <reason>", and returns the
// reason.
func optOut(line string) (string, error) {
	rest, _ := strings.CutPrefix(line, controlPrefix)
	reason, ok := strings.CutPrefix(strings.TrimSpace(rest), "none:")
	if !ok || len(strings.Fields(reason)) < minWords {
		return "", fmt.Errorf("%q: write control: none: <why this test cannot fail on the merge base>, in words", line)
	}
	return strings.TrimSpace(reason), nil
}
