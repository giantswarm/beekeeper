package testcontrol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// A test's run on a tree.
const (
	Fails   = "fails"
	Passes  = "passes"
	Skipped = "is skipped"
	NoBuild = "does not build"
	// Crashed is a package whose test binary fails before the test runs:
	// its TestMain or an init.
	Crashed = "fails before it runs"
	NotRun  = "does not run"
)

// DefaultTimeout is go test's -timeout for each run: a test that hangs on
// the base fails there once it passes.
const DefaultTimeout = 3 * time.Minute

// detailLines bounds a run's detail in the report.
const detailLines = 15

// go test -json's actions.
const (
	actionRun         = "run"
	actionPass        = "pass"
	actionFail        = "fail"
	actionSkip        = "skip"
	actionOutput      = "output"
	actionBuildOutput = "build-output"
	actionBuildFail   = "build-fail"
)

// event is a line of go test -json (cmd/test2json), with the build events
// of Go 1.24 and later.
type event struct {
	Action      string
	Package     string
	Test        string
	Output      string
	ImportPath  string
	FailedBuild string
}

// testRun is a top-level test's events: ran says it started, action is
// its pass, fail or skip.
type testRun struct {
	ran    bool
	action string
	out    []string
}

// pkgRun is a package's events outside its tests.
type pkgRun struct {
	action      string
	failedBuild string
	out         []string
}

// results are the events of one go test run.
type results struct {
	tests  map[[2]string]*testRun
	pkgs   map[string]*pkgRun
	builds map[string][]string
	// failed says the go command failed; stderr is what it wrote there.
	failed bool
	stderr string
}

// parseEvents reads go test -json output; a line that is not an event is
// kept as output of the run.
func parseEvents(r io.Reader) (*results, error) {
	res := &results{tests: map[[2]string]*testRun{}, pkgs: map[string]*pkgRun{}, builds: map[string][]string{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 16<<20)
	var stray []string
	for sc.Scan() {
		var e event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.Action == "" {
			stray = append(stray, sc.Text())
			continue
		}
		res.add(e)
	}
	if len(stray) > 0 {
		res.stderr = strings.Join(stray, "\n")
	}
	return res, sc.Err()
}

func (r *results) add(e event) {
	switch e.Action {
	case actionBuildOutput:
		r.builds[e.ImportPath] = append(r.builds[e.ImportPath], e.Output)
		return
	case actionBuildFail:
		if _, ok := r.builds[e.ImportPath]; !ok {
			r.builds[e.ImportPath] = nil
		}
		return
	}
	if e.Test == "" {
		p := r.pkg(e.Package)
		switch e.Action {
		case actionPass, actionFail, actionSkip:
			p.action = e.Action
			if e.FailedBuild != "" {
				p.failedBuild = e.FailedBuild
			}
		case actionOutput:
			p.out = append(p.out, e.Output)
		}
		return
	}
	top, _, sub := strings.Cut(e.Test, "/")
	key := [2]string{e.Package, top}
	t := r.tests[key]
	if t == nil {
		t = &testRun{}
		r.tests[key] = t
	}
	switch {
	case e.Action == actionOutput:
		if !strings.HasPrefix(e.Output, "=== ") {
			t.out = append(t.out, e.Output)
		}
	case sub:
	case e.Action == actionRun:
		t.ran = true
	case e.Action == actionPass || e.Action == actionFail || e.Action == actionSkip:
		t.action = e.Action
	}
}

func (r *results) pkg(name string) *pkgRun {
	p := r.pkgs[name]
	if p == nil {
		p = &pkgRun{}
		r.pkgs[name] = p
	}
	return p
}

// verdict is the run of the test name of the package pkg, and its detail.
func (r *results) verdict(pkg, name string) (string, string) {
	t := r.tests[[2]string{pkg, name}]
	p := r.pkgs[pkg]
	switch {
	case t != nil && t.action == actionFail:
		return Fails, clip(t.out)
	case t != nil && t.action == actionPass:
		return Passes, ""
	case t != nil && t.action == actionSkip:
		return Skipped, clip(t.out)
	case t != nil && t.ran && p != nil && p.action == actionFail:
		return Fails, "the test binary ended during the test (a panic, an exit or the timeout):\n" + clip(append(slices.Clone(t.out), p.out...))
	case p != nil && (p.failedBuild != "" || buildFailed(p.out)):
		return NoBuild, r.buildOutput(pkg, p)
	case p == nil && r.failed:
		if out := r.buildOutput(pkg, nil); out != "" {
			return NoBuild, out
		}
		return NoBuild, clip(strings.SplitAfter(r.stderr, "\n"))
	case p != nil && p.action == actionFail:
		return Crashed, clip(p.out)
	case p != nil:
		return NotRun, clip(p.out)
	}
	return NotRun, clip(strings.SplitAfter(r.stderr, "\n"))
}

// buildFailed says whether a package's output reports a failed build or
// setup.
func buildFailed(out []string) bool {
	return slices.ContainsFunc(out, func(l string) bool {
		return strings.Contains(l, "[build failed]") || strings.Contains(l, "[setup failed]")
	})
}

// buildOutput is the build errors of a package's test binary.
func (r *results) buildOutput(pkg string, p *pkgRun) string {
	if p != nil && p.failedBuild != "" && len(r.builds[p.failedBuild]) > 0 {
		return clip(r.builds[p.failedBuild])
	}
	var keys []string
	for k := range r.builds {
		if k == pkg || strings.HasPrefix(k, pkg+" ") {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	var out []string
	for _, k := range keys {
		out = append(out, r.builds[k]...)
	}
	if len(out) == 0 && p != nil {
		out = p.out
	}
	return clip(out)
}

// clip joins a run's output for the report: its first detailLines lines.
func clip(out []string) string {
	var lines []string
	for _, o := range out {
		for l := range strings.SplitSeq(strings.TrimRight(o, "\n"), "\n") {
			if strings.TrimSpace(l) != "" {
				lines = append(lines, strings.TrimRight(l, " \t\r"))
			}
		}
	}
	if len(lines) > detailLines {
		lines = append(lines[:detailLines], fmt.Sprintf("… %d more line(s)", len(lines)-detailLines))
	}
	return strings.Join(lines, "\n")
}

// runner runs cases with go test on a tree.
type runner struct {
	// module is the tree's module path; timeout is each run's -timeout.
	module  string
	timeout time.Duration
	// logs, when set, receives each run's events and stderr; runs counts
	// them.
	logs string
	runs int
}

// importPath is the import path of the directory dir of the module.
func (rn *runner) importPath(dir string) string {
	if dir == "." {
		return rn.module
	}
	return rn.module + "/" + dir
}

// pkgArg is go test's argument for the package in the directory dir.
func pkgArg(dir string) string {
	if dir == "." {
		return dir
	}
	return "./" + dir
}

// run runs the cases on the tree root, one go test per set of tags, and
// returns each case's run and detail by index.
func (rn *runner) run(ctx context.Context, root, label string, cases []*Case) ([]string, []string, error) {
	verdicts, details := make([]string, len(cases)), make([]string, len(cases))
	groups := map[string][]int{}
	var order []string
	for i, c := range cases {
		k := strings.Join(c.Tags, ",")
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], i)
	}
	for _, tags := range order {
		idx := groups[tags]
		var names, dirs []string
		for _, i := range idx {
			if !slices.Contains(names, cases[i].Name) {
				names = append(names, cases[i].Name)
			}
			if d := pkgArg(cases[i].Dir); !slices.Contains(dirs, d) {
				dirs = append(dirs, d)
			}
		}
		slices.Sort(names)
		slices.Sort(dirs)
		args := []string{"test", "-json", "-count=1", "-vet=off", "-timeout=" + rn.timeout.String()}
		if tags != "" {
			args = append(args, "-tags="+tags)
		}
		args = append(args, "-run=^(?:"+strings.Join(names, "|")+")$")
		args = append(args, dirs...)
		res, err := rn.goTest(ctx, root, label, args)
		if err != nil {
			return nil, nil, err
		}
		for _, i := range idx {
			verdicts[i], details[i] = res.verdict(rn.importPath(cases[i].Dir), cases[i].Name)
		}
	}
	return verdicts, details, nil
}

// goTest runs go with args in root and reads its events.
func (rn *runner) goTest(ctx context.Context, root, label string, args []string) (*results, error) {
	rn.runs++
	c := exec.CommandContext(ctx, "go", args...) //nolint:gosec // go test of the control's own trees
	c.Dir = root
	c.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "BEEKEEPER_TEST_CONTROL_")
	}), "GOWORK=off")
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	runErr := c.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("go %s in %s: %w", strings.Join(args, " "), root, ctx.Err())
	}
	if rn.logs != "" {
		base := filepath.Join(rn.logs, fmt.Sprintf("%s-%d", label, rn.runs))
		cmd := "go " + strings.Join(args, " ") + "\n"
		for name, data := range map[string][]byte{".cmd": []byte(cmd), ".json": stdout.Bytes(), ".stderr": stderr.Bytes()} {
			if err := os.WriteFile(base+name, data, 0o600); err != nil {
				return nil, err
			}
		}
	}
	res, err := parseEvents(&stdout)
	if err != nil {
		return nil, err
	}
	res.failed = runErr != nil
	if s := strings.TrimSpace(stderr.String()); s != "" {
		res.stderr = strings.TrimSpace(res.stderr + "\n" + s)
	}
	return res, nil
}

// modulePath reads the module path of the tree root's go.mod.
func modulePath(root string) (string, error) {
	gomod := filepath.Join(root, "go.mod")
	raw, err := os.ReadFile(gomod) //nolint:gosec // the tree's go.mod
	if err != nil {
		return "", err
	}
	for l := range strings.SplitSeq(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(l), "module"); ok && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
			return strings.Trim(strings.TrimSpace(rest), `"`), nil
		}
	}
	return "", fmt.Errorf("%s: no module line", gomod)
}
