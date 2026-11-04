package cmd

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
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
// policy after the check completes. A non-zero code refuses the merge with
// the reason; with code 0 a non-empty reason is a note for the merge's
// document: the validation could not run and the PR's own checks stand.
func (a *app) checkReviewedMergeResult(ctx context.Context, p superloop.Pull, head string) (string, int) {
	checked, reason, code := reviewedMergeCheck(ctx, a, p, head)
	if code != 0 {
		return reason, code
	}
	if !checked {
		return reason, 0
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

const reasonMergeValidationUnavailable = "merge-result validation unavailable"

// mergeValidationUnavailable is the refusal naming the condition that failed.
func mergeValidationUnavailable(format string, args ...any) string {
	return reasonMergeValidationUnavailable + ": " + fmt.Sprintf(format, args...)
}

// checkReviewedMerge builds p.Base + head in a detached worktree when p.Base
// is not already an ancestor of head, and runs lint and tests on the affected
// Go packages of the combined tree. A repository with no local checkout is
// not validated: the reason says so with code 0, and the PR's own green
// checks stand, as for a base that is already an ancestor of head.
func checkReviewedMerge(ctx context.Context, a *app, p superloop.Pull, head string) (bool, string, int) {
	if p.Base == "" {
		return false, mergeValidationUnavailable("pull request %s has no base branch", p.Ref), github.SquashTooling
	}
	if head == "" {
		return false, mergeValidationUnavailable("pull request %s has no reviewed head", p.Ref), github.SquashTooling
	}
	ref, ok := parseRef(p.Ref)
	if !ok {
		return false, mergeValidationUnavailable("pull request reference %q does not parse", p.Ref), github.SquashTooling
	}
	repoDir, searched := a.findReviewedRepoDir(ctx, ref.Repo)
	if repoDir == "" {
		return false, fmt.Sprintf("merge-result validation skipped: no checkout of %s in %s; the pull request's own checks stand", ref.Repo, strings.Join(searched, ", ")), 0
	}
	baseSHA, err := fetchReviewedMergeRefs(ctx, repoDir, p.Base, head, ref.N)
	if err != nil {
		return false, "fetching merge-result refs: " + err.Error(), github.SquashTooling
	}
	if baseSHA == "" {
		return false, mergeValidationUnavailable("base %s did not resolve in %s", p.Base, repoDir), github.SquashTooling
	}
	if !hasReviewedCommit(ctx, repoDir, head) {
		return false, mergeValidationUnavailable("reviewed head %s is not in %s", shortSHA(head), repoDir), github.SquashTooling
	}
	// When the current base is already an ancestor of head, the PR's own
	// green checks already verified the merge result.
	if _, err := runGit(ctx, repoDir, "merge-base", "--is-ancestor", baseSHA, head); err == nil {
		return false, "", 0
	}
	mbOut, err := runGit(ctx, repoDir, "merge-base", baseSHA, head)
	if err != nil {
		return true, mergeValidationUnavailable("reading merge base: %v", err), github.SquashTooling
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
		// Only unmerged index entries prove a branch conflict. Git can fail
		// because the checker cannot write, start a hook or create a commit.
		unmerged, readErr := runGit(ctx, tmpDir, "diff", "--name-only", "--diff-filter=U")
		if readErr == nil && strings.TrimSpace(string(unmerged)) != "" {
			return true, "branch conflicts with its base", github.SquashNotApplicable
		}
		return true, mergeValidationUnavailable("creating merge result: %v", err), github.SquashTooling
	}
	reason, code := runReviewedMergeChecks(ctx, tmpDir, mb, p.Base, head)
	return true, reason, code
}

// findReviewedRepoDir locates a local git checkout or worktree whose origin
// remote matches repo (owner/name), and lists the folders it searched. The
// folders of agents.dir and agents.roots are configured with a leading ~/,
// which is expanded here as agentDir does.
func (a *app) findReviewedRepoDir(ctx context.Context, repo string) (string, []string) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return "", nil
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
		r = filepath.Clean(homePath(r))
		candidates = append(candidates, r, filepath.Join(r, name), filepath.Join(r, owner, name))
	}
	candidates = slices.Compact(candidates)
	for _, d := range candidates {
		if matchesReviewedRepo(ctx, d, repo) {
			return d, candidates
		}
	}
	return "", candidates
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

// Resolve the exact fetched ref, never a cached tracking or local branch.
func fetchReviewedMergeRefs(ctx context.Context, repoDir, base, head string, prNumber int) (string, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// A unique ref prevents another fetch in this shared repository from
	// overwriting FETCH_HEAD while the lane resolves the validated base.
	fetchedRef := fmt.Sprintf("refs/beekeeper/merge-check/%d", time.Now().UnixNano())
	defer func() { _, _ = runGit(context.WithoutCancel(ctx), repoDir, "update-ref", "-d", fetchedRef) }()
	if err := runFetchGit(fetchCtx, repoDir, "fetch", "--no-write-fetch-head", "--no-tags", "--quiet", "origin", "refs/heads/"+base+":"+fetchedRef); err != nil {
		return "", err
	}
	out, err := runGit(ctx, repoDir, "rev-parse", "--verify", fetchedRef+"^{commit}")
	if err != nil {
		return "", err
	}
	baseSHA := strings.TrimSpace(string(out))
	if !hasReviewedCommit(ctx, repoDir, head) {
		if err := runFetchGit(fetchCtx, repoDir, "fetch", "--no-tags", "--quiet", "origin", fmt.Sprintf("refs/pull/%d/head", prNumber)); err != nil {
			return "", err
		}
	}
	if !hasReviewedCommit(ctx, repoDir, head) {
		return "", fmt.Errorf("reviewed head unavailable")
	}
	return baseSHA, nil
}

func runFetchGit(ctx context.Context, dir string, args ...string) error {
	c := proc.Command(proc.Background(ctx), "git", append([]string{"-C", dir, "-c", "credential.interactive=never"}, args...)...)
	c.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -oBatchMode=yes -oConnectTimeout=5", "LC_ALL=C", "LANGUAGE=C")
	return c.Run()
}

func hasReviewedCommit(ctx context.Context, repoDir, rev string) bool {
	_, err := runGit(ctx, repoDir, "cat-file", "-e", rev+"^{commit}")
	return err == nil
}

// runReviewedMergeChecks runs goconst/golangci-lint and go test on the Go
// module in worktreeDir, including reverse dependents and unchanged tests.
func runReviewedMergeChecks(ctx context.Context, worktreeDir, mergeBase, baseBranch, head string) (string, int) {
	if _, err := os.Stat(filepath.Join(worktreeDir, "go.mod")); err != nil {
		return "", 0
	}
	// Whole-module checks include unchanged tests, reverse dependents and
	// dependency-only changes. Filename correspondence cannot prove coverage.
	pkgs := []string{"./..."}
	if _, err := exec.LookPath("golangci-lint"); err != nil {
		return "merge-result golangci-lint unavailable: " + err.Error(), github.SquashTooling
	}
	// Keep the fast diagnostic, but it never substitutes for configured lint.
	diffOut, err := runGit(ctx, worktreeDir, "diff", "--name-only", mergeBase+"..HEAD")
	if err != nil {
		return "reading merge diff: " + err.Error(), github.SquashTooling
	}
	changedFiles := strings.Fields(string(diffOut))
	for _, pkg := range changedGoPackages(worktreeDir, changedFiles) {
		if finding := checkPackageGoConst(worktreeDir, pkg); finding != "" {
			return fmt.Sprintf("merge result (%s + %s) failed golangci-lint: %s", baseBranch, shortSHA(head), finding), github.SquashRed
		}
	}
	lintArgs := strings.Fields("run --max-issues-per-linter=0 --max-same-issues=0")
	if !hasGolangCILintConfig(worktreeDir) {
		lintArgs = append(lintArgs, "--enable", "goconst,govet,gosec")
	}
	lintArgs = append(lintArgs, pkgs...)
	if out, err := runMergeCheckCmd(ctx, worktreeDir, "golangci-lint", lintArgs...); err != nil {
		if errors.Is(err, errMergeCheckEnvironment) {
			return mergeValidationUnavailable("%v", err), github.SquashTooling
		}
		return fmt.Sprintf("merge result (%s + %s) failed golangci-lint: %s", baseBranch, shortSHA(head), summarizeMergeCheckOutput(out)), github.SquashRed
	}
	if out, err := runMergeCheckCmd(ctx, worktreeDir, "go", goTestSubcmd, "./..."); err != nil {
		if errors.Is(err, errMergeCheckEnvironment) {
			return mergeValidationUnavailable("%v", err), github.SquashTooling
		}
		return fmt.Sprintf("merge result (%s + %s) failed go test: %s", baseBranch, shortSHA(head), summarizeMergeCheckOutput(out)), github.SquashRed
	}

	return "", 0
}

// checkPackageGoConst checks a Go package directory for repeated string
// literals (>=3 occurrences of length >=3, or matching an existing constant)
// as an early diagnostic; the configured golangci-lint remains required.
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

// mergeCheckTimeout bounds one merge-result check command: a check that
// cannot finish refuses the merge rather than holding the lane and the
// machine.
var mergeCheckTimeout = 30 * time.Minute

func runMergeCheckCmd(ctx context.Context, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, mergeCheckTimeout)
	defer cancel()
	var buf bytes.Buffer
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed toolchain command in temporary worktree
	c.Dir = dir
	// Git diagnostics and tests must not inherit the desk's language.
	c.Env = append(os.Environ(), "LC_ALL=C", "LANGUAGE=C")
	c.Stdout, c.Stderr = &buf, &buf
	mergeCheckGroup(c)
	c.WaitDelay = 5 * time.Second
	err := c.Run()
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("%w: %s after %s", errMergeCheckTimeout, name, mergeCheckTimeout)
	}
	if err != nil {
		var exit *exec.ExitError
		// Starting a missing/unusable tool, cancellation and a killed process
		// describe this checker, not a defect in the reviewed branch.
		if ctx.Err() != nil || !errors.As(err, &exit) || exit.ExitCode() < 0 || mergeCheckHostFailure(buf.String()) {
			err = fmt.Errorf("%w: %w: %s", errMergeCheckEnvironment, err, summarizeMergeCheckOutput(buf.String()))
		}
	}
	return buf.String(), err
}

// These toolchain diagnostics identify host resource failures even when the
// top-level go/lint command exits normally with status 1 or 2.
func mergeCheckHostFailure(out string) bool {
	for _, diagnostic := range []string{
		"no space left on device", "disk quota exceeded",
		"cannot allocate memory", "signal: killed",
		"resource temporarily unavailable", "too many open files",
		"runtime: failed to create new OS thread",
	} {
		if strings.Contains(out, diagnostic) {
			return true
		}
	}
	return false
}

var (
	errMergeCheckTimeout     = errors.New("merge-result check timed out")
	errMergeCheckEnvironment = errors.New("merge-result check environment failed")
)

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
