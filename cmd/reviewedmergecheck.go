package cmd

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	// The merge commit is thrown away, so the machine's signing setup never applies.
	if _, err := runGit(ctx, tmpDir, "-c", "user.name=Beekeeper", "-c", "user.email=beekeeper@giantswarm.io", "-c", "commit.gpgsign=false", gitMerge, "--no-ff", "--no-edit", head); err != nil {
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

// runReviewedMergeChecks runs repository-configured golangci-lint and go test on the Go
// module in worktreeDir, including reverse dependents and unchanged tests.
func runReviewedMergeChecks(ctx context.Context, worktreeDir, _, baseBranch, head string) (string, int) {
	if _, err := os.Stat(filepath.Join(worktreeDir, "go.mod")); err != nil {
		return "", 0
	}
	// Whole-module checks include unchanged tests, reverse dependents and
	// dependency-only changes. Filename correspondence cannot prove coverage.
	pkgs := []string{"./..."}
	if _, err := exec.LookPath("golangci-lint"); err != nil {
		return "merge-result golangci-lint unavailable: " + err.Error(), github.SquashTooling
	}
	// Never approximate a repository's linter policy with a separate check or
	// extra linters. Pin the config to this merge result so discovery cannot
	// inherit a configuration from a parent directory or the user's home.
	configName := golangCILintConfig(worktreeDir)
	lintArgs := []string{"run", "--max-issues-per-linter=0", "--max-same-issues=0"}
	configLabel := "no repository config (--no-config; default linters)"
	if configName == "" {
		lintArgs = append(lintArgs, "--no-config")
	} else {
		lintArgs = append(lintArgs, "--config", filepath.Join(worktreeDir, configName))
		configLabel = configName
	}
	// Independent merge worktrees may lint concurrently with other workers.
	// Avoid the host-wide runner lock without suppressing lint findings.
	lintArgs = append(lintArgs, "--allow-parallel-runners")
	lintArgs = append(lintArgs, pkgs...)
	if out, err := runMergeCheckCmd(ctx, worktreeDir, "golangci-lint", lintArgs...); err != nil {
		if errors.Is(err, errMergeCheckEnvironment) {
			return mergeValidationUnavailable("%v", err), github.SquashTooling
		}
		return fmt.Sprintf("merge result (%s + %s) failed golangci-lint (config: %s): %s", baseBranch, shortSHA(head), configLabel, summarizeMergeCheckOutput(out)), github.SquashRed
	}
	// Keep heavyweight live canaries in full CI, outside the coordinator merge
	// path. Short mode still checks every package and its reverse dependents.
	if out, err := runMergeCheckCmd(ctx, worktreeDir, "go", goTestSubcmd, "-short", "./..."); err != nil {
		if errors.Is(err, errMergeCheckEnvironment) {
			return mergeValidationUnavailable("%v", err), github.SquashTooling
		}
		return fmt.Sprintf("merge result (%s + %s) failed go test: %s", baseBranch, shortSHA(head), summarizeMergeCheckOutput(out)), github.SquashRed
	}

	return "", 0
}

const (
	golangCILintYML  = ".golangci.yml"
	golangCILintYAML = ".golangci.yaml"
	golangCILintTOML = ".golangci.toml"
	golangCILintJSON = ".golangci.json"
)

// golangCILintConfig follows golangci-lint's config extension precedence.
func golangCILintConfig(dir string) string {
	for _, name := range []string{golangCILintYML, golangCILintYAML, golangCILintTOML, golangCILintJSON} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return name
		}
	}
	return ""
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
