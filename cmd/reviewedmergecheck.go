package cmd

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/superloop"
	"github.com/giantswarm/beekeeper/internal/worktrees"
)

// reviewedMergeCheck builds PR + current base in a temporary worktree and
// runs CI checks (lint and go test) on the merge result before the base
// branch moves; a seam for tests.
var reviewedMergeCheck = checkReviewedMerge

// checkReviewedMergeResult verifies that merging head into p.Base is green
// when p.Base has moved ahead of head, and re-checks desk stop and hold
// policy after the check completes.
func (a *app) checkReviewedMergeResult(ctx context.Context, p superloop.Pull, head string) (string, int) {
	checked, reason, code := reviewedMergeCheck(ctx, a, p, head)
	if code != 0 {
		return reason, code
	}
	if !checked {
		return "", 0
	}
	st, err := a.store.Read()
	if err != nil {
		return err.Error(), github.SquashNotApplicable
	}
	if st.Stop != nil {
		return reasonDeskStopped, github.SquashNotApplicable
	}
	if ref, ok := parseRef(p.Ref); ok {
		if _, held := merge.BlockingFor(st, time.Now(), loopParty.Name, ref.Repo, ref.N, a.cfg.LaneOf(ref.Repo)); held {
			return reasonMergeHeld, github.SquashNotApplicable
		}
	}
	return "", 0
}

// checkReviewedMerge builds p.Base + head in a detached worktree when p.Base
// is not already an ancestor of head, and runs lint and tests on the affected
// Go packages of the combined tree.
func checkReviewedMerge(ctx context.Context, a *app, p superloop.Pull, head string) (bool, string, int) {
	if p.Base == "" || head == "" {
		return false, "", 0
	}
	ref, ok := parseRef(p.Ref)
	if !ok {
		return false, "", 0
	}
	repoDir := a.findReviewedRepoDir(ctx, ref.Repo)
	if repoDir == "" {
		return false, "", 0
	}
	fetchReviewedMergeRefs(ctx, repoDir, p.Base, head, ref.N)
	baseSHA := resolveReviewedBaseCommit(ctx, repoDir, p.Base)
	if baseSHA == "" || !hasReviewedCommit(ctx, repoDir, head) {
		return false, "", 0
	}
	// When the current base is already an ancestor of head, the PR's own
	// green checks already verified the merge result.
	if _, err := runGit(ctx, repoDir, "merge-base", "--is-ancestor", baseSHA, head); err == nil {
		return false, "", 0
	}
	mbOut, err := runGit(ctx, repoDir, "merge-base", baseSHA, head)
	if err != nil {
		return true, "branch conflicts with its base", github.SquashNotApplicable
	}
	mb := strings.TrimSpace(string(mbOut))

	stateDir := a.cfg.StateDir
	if stateDir == "" && a.store != nil {
		stateDir = a.store.Dir()
	}
	parentDir, err := filepath.Abs(filepath.Join(cmp.Or(stateDir, os.TempDir()), "merge-check"))
	if err != nil {
		return true, "resolving merge-check directory: " + err.Error(), github.SquashTooling
	}
	if err := os.MkdirAll(parentDir, 0o700); err != nil {
		return true, "creating merge-check directory: " + err.Error(), github.SquashTooling
	}
	tmpDir, err := os.MkdirTemp(parentDir, "pr-*")
	if err != nil {
		return true, "creating merge-check worktree: " + err.Error(), github.SquashTooling
	}
	defer func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = runGit(cleanCtx, repoDir, "worktree", loopRemove, "--force", tmpDir)
		_ = os.RemoveAll(tmpDir) //nolint:gosec // owned temporary worktree under state directory
	}()
	if _, err := runGit(ctx, repoDir, "worktree", loopAdd, "--force", "--detach", tmpDir, baseSHA); err != nil {
		return true, "creating merge-check worktree: " + err.Error(), github.SquashTooling
	}
	if _, err := runGit(ctx, tmpDir, "-c", "user.name=Beekeeper", "-c", "user.email=beekeeper@giantswarm.io", gitMerge, "--no-ff", "--no-edit", head); err != nil {
		return true, "branch conflicts with its base", github.SquashNotApplicable
	}
	reason, code := runReviewedMergeChecks(ctx, tmpDir, mb, p.Base, head)
	return true, reason, code
}

// findReviewedRepoDir locates a local git checkout or worktree whose origin
// remote matches repo (owner/name).
func (a *app) findReviewedRepoDir(ctx context.Context, repo string) string {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return ""
	}
	var roots []string
	roots = append(roots, a.cfg.Disk.Checkouts...)
	if a.cfg.Agents.Dir != "" {
		roots = append(roots, a.cfg.Agents.Dir)
	}
	roots = append(roots, a.cfg.Agents.Roots...)
	var candidates []string
	for _, r := range roots {
		if r == "" {
			continue
		}
		candidates = append(candidates, r, filepath.Join(r, name), filepath.Join(r, owner, name))
	}
	for _, d := range candidates {
		if matchesReviewedRepo(ctx, d, repo) {
			return d
		}
	}
	return ""
}

func matchesReviewedRepo(ctx context.Context, dir, repo string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	for _, args := range [][]string{
		{"remote", "get-url", "origin"},
		{"remote", "get-url", "--push", "origin"},
	} {
		out, err := runGit(ctx, dir, args...)
		if err == nil && strings.EqualFold(worktrees.RepoOf(strings.TrimSpace(string(out))), repo) {
			return true
		}
	}
	return false
}

func fetchReviewedMergeRefs(ctx context.Context, repoDir, base, head string, prNumber int) {
	fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	runFetchGit(fetchCtx, repoDir, "fetch", "--no-tags", "--quiet", "origin", base)
	if hasReviewedCommit(ctx, repoDir, head) {
		return
	}
	runFetchGit(fetchCtx, repoDir, "fetch", "--no-tags", "--quiet", "origin", head)
	if !hasReviewedCommit(ctx, repoDir, head) && prNumber > 0 {
		runFetchGit(fetchCtx, repoDir, "fetch", "--no-tags", "--quiet", "origin", fmt.Sprintf("refs/pull/%d/head", prNumber))
	}
}

func runFetchGit(ctx context.Context, dir string, args ...string) {
	c := proc.Command(proc.Background(ctx), "git", append([]string{"-C", dir, "-c", "credential.interactive=never"}, args...)...)
	c.Env = append(os.Environ(),
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes -oConnectTimeout=5",
	)
	_ = c.Run()
}

func resolveReviewedBaseCommit(ctx context.Context, repoDir, base string) string {
	for _, ref := range []string{"refs/remotes/origin/" + base, "refs/heads/" + base, base} {
		out, err := runGit(ctx, repoDir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		if err == nil {
			if sha := strings.TrimSpace(string(out)); sha != "" {
				return sha
			}
		}
	}
	return ""
}

func hasReviewedCommit(ctx context.Context, repoDir, rev string) bool {
	_, err := runGit(ctx, repoDir, "cat-file", "-e", rev+"^{commit}")
	return err == nil
}

var testFuncPattern = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\s*\(`)

// runReviewedMergeChecks runs goconst/golangci-lint and go test on the Go
// packages in worktreeDir touched between mergeBase and HEAD.
func runReviewedMergeChecks(ctx context.Context, worktreeDir, mergeBase, baseBranch, head string) (string, int) {
	if _, err := os.Stat(filepath.Join(worktreeDir, "go.mod")); err != nil {
		return "", 0
	}
	diffOut, err := runGit(ctx, worktreeDir, "diff", "--name-only", mergeBase+"..HEAD")
	if err != nil {
		return "reading merge diff: " + err.Error(), github.SquashTooling
	}
	changedFiles := strings.Fields(string(diffOut))
	pkgs := changedGoPackages(worktreeDir, changedFiles)
	if len(pkgs) == 0 {
		return "", 0
	}
	for _, pkg := range pkgs {
		if finding := checkPackageGoConst(worktreeDir, pkg); finding != "" {
			return fmt.Sprintf("merge result (%s + %s) failed golangci-lint: %s", baseBranch, shortSHA(head), finding), github.SquashRed
		}
	}
	if _, err := exec.LookPath("golangci-lint"); err == nil {
		lintArgs := strings.Fields("run --max-issues-per-linter=0 --max-same-issues=0")
		if !hasGolangCILintConfig(worktreeDir) {
			lintArgs = append(lintArgs, "-E", "goconst,govet,gosec")
		}
		lintArgs = append(lintArgs, pkgs...)
		if out, err := runMergeCheckCmd(ctx, worktreeDir, "golangci-lint", lintArgs...); err != nil {
			return fmt.Sprintf("merge result (%s + %s) failed golangci-lint: %s", baseBranch, shortSHA(head), summarizeMergeCheckOutput(out)), github.SquashRed
		}
	}
	for _, pkg := range pkgs {
		testArgs := goTestArgsForPackage(worktreeDir, pkg, changedFiles)
		if out, err := runMergeCheckCmd(ctx, worktreeDir, "go", testArgs...); err != nil {
			return fmt.Sprintf("merge result (%s + %s) failed go test: %s", baseBranch, shortSHA(head), summarizeMergeCheckOutput(out)), github.SquashRed
		}
	}
	return "", 0
}

// checkPackageGoConst checks a Go package directory for repeated string
// literals (>=3 occurrences of length >=3, or matching an existing constant)
// so goconst regressions across merged PRs are caught even when golangci-lint
// is not installed in the runner image.
func checkPackageGoConst(worktreeDir, pkg string) string {
	relDir := strings.TrimPrefix(pkg, "./")
	if pkg == "." {
		relDir = "."
	}
	absDir := filepath.Join(worktreeDir, filepath.FromSlash(relDir))
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return ""
	}
	for _, testPass := range []bool{false, true} {
		if finding := checkFilesGoConst(worktreeDir, absDir, entries, testPass); finding != "" {
			return finding
		}
	}
	return ""
}

func checkFilesGoConst(worktreeDir, absDir string, entries []os.DirEntry, testPass bool) string {
	fset := token.NewFileSet()
	consts := map[string]string{}
	type occurrence struct {
		pos   token.Position
		count int
	}
	counts := map[string]*occurrence{}
	var order []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		isTest := strings.HasSuffix(e.Name(), "_test.go")
		if isTest != testPass {
			continue
		}
		path := filepath.Join(absDir, e.Name())
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			continue
		}
		ignore := map[*ast.BasicLit]bool{}
		for _, imp := range f.Imports {
			if imp.Path != nil {
				ignore[imp.Path] = true
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			gd, ok := n.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				return true
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, val := range vs.Values {
					lit, ok := val.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					ignore[lit] = true
					if s, err := strconv.Unquote(lit.Value); err == nil && len(s) >= 3 && i < len(vs.Names) {
						consts[s] = vs.Names[i].Name
					}
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			for _, lit := range goconstLiterals(n, ignore) {
				s, err := strconv.Unquote(lit.Value)
				if err != nil || len(s) < 3 {
					continue
				}
				occ, exists := counts[s]
				if !exists {
					occ = &occurrence{pos: fset.Position(lit.Pos())}
					counts[s] = occ
					order = append(order, s)
				}
				occ.count++
			}
			return true
		})
	}
	for _, s := range order {
		occ := counts[s]
		if occ.count < 3 {
			continue
		}
		rel, err := filepath.Rel(worktreeDir, occ.pos.Filename)
		if err != nil {
			rel = filepath.Base(occ.pos.Filename)
		}
		if name, ok := consts[s]; ok {
			return fmt.Sprintf("%s:%d:%d: string %q has %d occurrences, already defined as %s (goconst)", filepath.ToSlash(rel), occ.pos.Line, occ.pos.Column, s, occ.count, name)
		}
		return fmt.Sprintf("%s:%d:%d: string %q has %d occurrences, make it a constant (goconst)", filepath.ToSlash(rel), occ.pos.Line, occ.pos.Column, s, occ.count)
	}
	return ""
}

// goconstLiterals extracts string literals from the AST node types that
// jgautheron/goconst inspects (equality comparisons, assignments, variable
// specs, return statements, and case clauses).
func goconstLiterals(n ast.Node, ignore map[*ast.BasicLit]bool) []*ast.BasicLit {
	var exprs []ast.Expr
	switch v := n.(type) {
	case *ast.BinaryExpr:
		if v.Op == token.EQL || v.Op == token.NEQ {
			exprs = append(exprs, v.X, v.Y)
		}
	case *ast.AssignStmt:
		exprs = append(exprs, v.Rhs...)
	case *ast.GenDecl:
		if v.Tok == token.VAR {
			for _, spec := range v.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					exprs = append(exprs, vs.Values...)
				}
			}
		}
	case *ast.ReturnStmt:
		exprs = append(exprs, v.Results...)
	case *ast.CaseClause:
		exprs = append(exprs, v.List...)
	case *ast.CompositeLit:
		exprs = append(exprs, v.Elts...)
	case *ast.KeyValueExpr:
		exprs = append(exprs, v.Key)
		if _, isIdent := v.Key.(*ast.Ident); !isIdent {
			exprs = append(exprs, v.Value)
		}
	}
	var out []*ast.BasicLit
	for _, e := range exprs {
		if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING && !ignore[lit] {
			out = append(out, lit)
		}
	}
	return out
}

func hasGolangCILintConfig(dir string) bool {
	for _, name := range []string{".golangci.yaml", ".golangci.yml", ".golangci.toml", ".golangci.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// changedGoPackages returns the sorted relative package paths (./ or ./sub/pkg)
// in worktreeDir containing .go files that were modified in changedFiles.
func changedGoPackages(worktreeDir string, changedFiles []string) []string {
	var dirs []string
	for _, f := range changedFiles {
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		d := filepath.Dir(filepath.FromSlash(f))
		pkg := "./" + filepath.ToSlash(d)
		if d == "." {
			pkg = "."
		}
		if hasGoFiles(filepath.Join(worktreeDir, d)) {
			dirs = append(dirs, pkg)
		}
	}
	slices.Sort(dirs)
	return slices.Compact(dirs)
}

func hasGoFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}

const goTestSubcmd = "test"

// goTestArgsForPackage builds the `go test` invocation for pkg. Small packages
// run their full test suite; huge packages (>30 *_test.go files, such as ./cmd)
// compile all tests and run the tests in the changed files and their matching
// *_test.go counterparts.
func goTestArgsForPackage(worktreeDir, pkg string, changedFiles []string) []string {
	relDir := strings.TrimPrefix(pkg, "./")
	if pkg == "." {
		relDir = "."
	}
	absDir := filepath.Join(worktreeDir, filepath.FromSlash(relDir))
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return []string{goTestSubcmd, pkg}
	}
	var testFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), "_test.go") {
			testFiles = append(testFiles, e.Name())
		}
	}
	if len(testFiles) <= 30 {
		return []string{goTestSubcmd, pkg}
	}
	names := matchingTestFuncs(absDir, relDir, testFiles, changedFiles)
	if len(names) == 0 {
		return []string{goTestSubcmd, "-run", "^$", pkg}
	}
	return []string{goTestSubcmd, "-run", "^(" + strings.Join(names, "|") + ")$", pkg}
}

func matchingTestFuncs(absDir, relDir string, testFiles, changedFiles []string) []string {
	var stems []string
	var selected []string
	for _, f := range changedFiles {
		d := filepath.ToSlash(filepath.Dir(filepath.FromSlash(f)))
		if d != relDir {
			continue
		}
		base := filepath.Base(f)
		if strings.HasSuffix(base, "_test.go") {
			selected = append(selected, base)
			continue
		}
		if stem, ok := strings.CutSuffix(base, ".go"); ok {
			stems = append(stems, stem)
		}
	}
	for _, tf := range testFiles {
		for _, stem := range stems {
			if tf == stem+"_test.go" || strings.HasPrefix(tf, stem+"_") {
				selected = append(selected, tf)
			}
		}
	}
	slices.Sort(selected)
	selected = slices.Compact(selected)
	var funcs []string
	for _, tf := range selected {
		raw, err := os.ReadFile(filepath.Join(absDir, tf)) //nolint:gosec // reading *_test.go file from owned worktree
		if err != nil {
			continue
		}
		for _, m := range testFuncPattern.FindAllSubmatch(raw, -1) {
			funcs = append(funcs, string(m[1]))
		}
	}
	slices.Sort(funcs)
	return slices.Compact(funcs)
}

func runMergeCheckCmd(ctx context.Context, dir, name string, args ...string) (string, error) {
	var buf bytes.Buffer
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed toolchain command in temporary worktree
	c.Dir = dir
	c.Stdout, c.Stderr = &buf, &buf
	err := c.Run()
	return buf.String(), err
}

func summarizeMergeCheckOutput(out string) string {
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "ok \t") || strings.HasPrefix(trimmed, "ok  ") || strings.HasPrefix(trimmed, "?   \t") {
			continue
		}
		kept = append(kept, trimmed)
	}
	s := oneLine(strings.Join(kept, "; "))
	if len(s) > 400 {
		s = s[len(s)-400:]
	}
	return s
}
