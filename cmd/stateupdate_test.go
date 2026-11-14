package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// updateSlowCalls is a source-level guard for explicit slow operations inside
// state-update callbacks. Import aliases are resolved, and a preconstructed
// process/client cannot hide a Run or Do call inside the transaction.
func updateSlowCalls(filename string, source any) ([]token.Position, error) {
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, filename, source, 0)
	if err != nil {
		return nil, err
	}
	imports := map[string]string{}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, err
		}
		name := filepath.Base(path)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = path
	}
	var positions []token.Position
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || method.Sel.Name != "Update" && method.Sel.Name != "UpdateWithin" {
			return true
		}
		for _, arg := range call.Args {
			callback, ok := arg.(*ast.FuncLit)
			if !ok {
				continue
			}
			ast.Inspect(callback.Body, func(n ast.Node) bool {
				inner, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				fn, ok := inner.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				bad := false
				if pkg, ok := fn.X.(*ast.Ident); ok {
					switch imports[pkg.Name] {
					case "os/exec", "net/http", "net":
						bad = true
					case "time", "github.com/giantswarm/beekeeper/internal/clock":
						bad = fn.Sel.Name == "Sleep" || fn.Sel.Name == "After" || fn.Sel.Name == "NewTimer"
					case "github.com/giantswarm/beekeeper/internal/github":
						bad = fn.Sel.Name == "RunGH"
					}
				}
				switch fn.Sel.Name {
				case "Run", "Output", "CombinedOutput", "Start", "Do", "RoundTrip":
					bad = true
				}
				if bad {
					positions = append(positions, set.Position(inner.Pos()))
				}
				return true
			})
		}
		return true
	})
	return positions, nil
}

// updateGuardedDirs are the packages whose state updates the guard checks:
// the commands and the supervisor loop, gate and watch they run.
var updateGuardedDirs = []string{".", filepath.Join("..", "internal", "gate"), filepath.Join("..", "internal", "loop"), filepath.Join("..", "internal", "watch")}

func TestUpdateCallbacksHaveNoExplicitSlowCalls(t *testing.T) {
	for _, dir := range updateGuardedDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			n++
			positions, err := updateSlowCalls(filepath.Join(dir, name), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range positions {
				t.Errorf("%s: move subprocess/network/sleep work before the state Update callback", p)
			}
		}
		if n == 0 {
			t.Errorf("%s holds no Go file: the guarded directories are stale", dir)
		}
	}
}

func TestUpdateCallbackGuardRejectsSubprocessNetworkAndSleep(t *testing.T) {
	for _, body := range []string{
		`process.Command("sh", "-c", "true").Run()`,
		`prepared.Run()`,
		`web.Get("https://example.invalid")`,
		`client.Do(request)`,
		`clock.Sleep(clock.Second)`,
	} {
		source := `package fixture
import (process "os/exec"; web "net/http"; clock "time")
func bad() { store.Update(func(st *State) ([]Event, error) { ` + body + `; return nil, nil }) }`
		positions, err := updateSlowCalls("fixture.go", source)
		if err != nil || len(positions) == 0 {
			t.Fatalf("accepted %q: %v, %v", body, positions, err)
		}
	}
	// The core's waits go through internal/clock, which a scenario's world
	// advances; they are as slow inside a callback.
	positions, err := updateSlowCalls("clock.go", `package fixture
import clk "github.com/giantswarm/beekeeper/internal/clock"
func bad() { store.Update(func(st *State) ([]Event, error) { <-clk.After(1); return nil, nil }) }`)
	if err != nil || len(positions) == 0 {
		t.Fatalf("accepted a wait on internal/clock: %v, %v", positions, err)
	}
	positions, err = updateSlowCalls("pure.go", `package fixture
func good() { store.Update(func(st *State) ([]Event, error) { st.Count++; return nil, nil }) }`)
	if err != nil || len(positions) != 0 {
		t.Fatalf("pure transition: %v, %v", positions, err)
	}
}
