package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/remotebuild"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

var mergeRemoteCheck = func(ctx context.Context, runner remotebuild.Runner, argv []string) (bool, int) {
	return runner.TryMergeCheck(ctx, argv)
}

var (
	mergeReadLoad = machine.ReadLoad
	mergeNumCPU   = runtime.NumCPU
	mergeReadDisk = machine.ReadDisk
)

// An overloaded coordinator never starts a full-module checker. The configured
// remote runner is selected before this guard and never falls back to local work.
var mergeLocalHeadroom = func(dir string) error {
	if load, err := mergeReadLoad(); err == nil {
		if cpus := mergeNumCPU(); load[0] >= float64(cpus) {
			return fmt.Errorf("coordinator overloaded (load %.1f, CPUs %d); configure run.remote or await merge-result CI", load[0], cpus)
		}
	}
	if disk, err := mergeReadDisk(dir); err == nil && disk.FreeMiB < 8*1024 {
		return fmt.Errorf("check filesystem has %d MiB free; at least 8192 MiB required", disk.FreeMiB)
	}
	return nil
}

// What the desk does after an environment finding of the merge-result check.
const (
	mergeEnvironmentRetry      = "retrying once in a clean environment, or accepting CI on the exact merge result"
	mergeEnvironmentDependency = "retrying cannot provide it, so the pull request's own checks stand; " +
		"provide it where the merge-result check runs (the desk host, or run.remote's builder image)"
)

func (a *app) recordMergeEnvironment(p superloop.Pull, head, reason, then string) error {
	if a.store == nil {
		return fmt.Errorf("no store for merge environment finding")
	}
	key := "merge environment " + p.Ref + "@" + head
	events, err := a.store.Events(0, func(e state.Event) bool {
		return e.Verb == loop.LoopFileVerb && strings.HasPrefix(e.Detail, "["+key+"] ")
	})
	if err != nil || len(events) > 0 {
		return err
	}
	ref, _ := parseRef(p.Ref)
	finding := fmt.Sprintf("Merge-result check environment failed for https://github.com/%s/pull/%d at head %s: %s. This is a checker environment finding, not a branch repair; %s.", ref.Repo, ref.N, head, reason, then)
	return a.store.Record(about(event(loop.LoopParty, loop.LoopFileVerb, "[%s] %s%s%s", key, loop.LoopParty.Name, loop.FileReportSeparator, finding), p.Ref))
}

func (a *app) runReviewedMergeChecks(ctx context.Context, dir, baseSHA, base, head string) (string, int) {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return "", 0
	}
	if a.cfg.Run.Remote.Enabled {
		var output bytes.Buffer
		runner := remotebuild.Runner{Dir: dir, Config: a.cfg.Run.Remote, StateDir: a.cfg.StateDir,
			Stdout: &output, Stderr: &output, Record: a.runRecorder(), Head: head}
		dirs := []string{dir}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// This attempt's worktree is finished, including after an environment
			// failure. A retry gets a new Job with fresh scratch and build caches.
			_ = runner.Sweep(cleanup, dirs, false)
		}()
		run := func(ctx context.Context, checkDir, name string, args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, mergeCheckTimeout)
			defer cancel()
			output.Reset()
			runner.Dir = checkDir
			runner.Head = head
			if checkDir != dir {
				runner.Head = baseSHA
				dirs = append(dirs, checkDir)
			}
			runner.Name = name + " " + strings.Join(args, " ")
			handled, rc := mergeRemoteCheck(ctx, runner, append([]string{name}, args...))
			out := output.String()
			if !handled || ctx.Err() != nil || rc == 125 || rc == 126 || rc == 127 || rc >= 128 || rc != 0 && mergeCheckHostFailure(out) {
				return out, fmt.Errorf("%w: remote checker exit %d: %s; %s", errMergeCheckEnvironment, rc, mergeCheckHostDiagnostic(out), summarizeMergeCheckOutput(out))
			}
			if rc != 0 {
				return out, fmt.Errorf("remote check exited %d", rc)
			}
			return out, nil
		}
		return runReviewedMergeChecksWith(ctx, dir, baseSHA, base, head, run, false)
	}
	if err := mergeLocalHeadroom(dir); err != nil {
		return mergeValidationUnavailable("%v", err), github.SquashTooling
	}
	// TMPDIR and the compiler/linter caches live on the checked filesystem,
	// outside the source snapshot, not in the desk's quota-limited default temp.
	scratch, err := os.MkdirTemp(filepath.Dir(dir), "environment-*")
	if err != nil {
		return mergeValidationUnavailable("creating check scratch: %v", err), github.SquashTooling
	}
	defer func() { _ = os.RemoveAll(scratch) }() //nolint:gosec // owned temporary checker environment
	env := []string{"TMPDIR=" + scratch, "GOTMPDIR=" + scratch, "GOCACHE=" + filepath.Join(scratch, "go-cache"), "GOLANGCI_LINT_CACHE=" + filepath.Join(scratch, "lint-cache"), "GOMAXPROCS=2"}
	run := func(ctx context.Context, dir, name string, args ...string) (string, error) {
		return runMergeCheckCmdEnv(ctx, dir, env, name, args...)
	}
	return runReviewedMergeChecksWith(ctx, dir, baseSHA, base, head, run, true)
}

// A native build dependency named by the C toolchain, pkg-config or the
// linker. Each pattern starts at the line's start with the tool's own prefix
// (a source location, the tool's name), so source text the compiler quotes,
// such as an #error directive, cannot pose as one.
var (
	mergeCheckMissingHeader = []*regexp.Regexp{
		regexp.MustCompile(`^[^\s:]+(?::\d+)*: fatal error: ([^\s:']+): No such file or directory`), // gcc
		regexp.MustCompile(`^[^\s:]+(?::\d+)*: fatal error: '([^']+)' file not found`),              // clang
	}
	mergeCheckMissingDependency = []struct {
		pattern *regexp.Regexp
		name    string
	}{
		{regexp.MustCompile(`^Package (\S+) was not found in the pkg-config search path`), "pkg-config package %s"},
		{regexp.MustCompile(`^\S*ld(?:\.\w+)?: cannot find -l([\w+.-]+)`), "library lib%s"},
		{regexp.MustCompile(`^(?:[\w./-]+: )?(?:C compiler "[^"]+" not found: )?exec: "([^"]+)": executable file not found in \$PATH`), "tool %s"},
	}
	// A package whose tests ran and failed, rather than one that did not build.
	goTestRanAndFailed = regexp.MustCompile(`^(--- FAIL|FAIL\s+\S+\s+[^\s\[])`)
)

// mergeCheckHostDependency names the native build dependencies the checker
// host lacks when they are all that failed in go test's output: no test ran
// and failed, and every build failure (a "# <package>" block) names a missing
// C header, pkg-config package, library or tool. The repository's CI has them,
// since the reviewed head and its base passed it. A header the repository
// itself tracks (tracked) is the merge result's own breakage, never the host's.
func mergeCheckHostDependency(out string, tracked func(string) bool) string {
	var missing []string
	block, blockMissing, blockOther := false, false, false
	// A block that reports anything but a missing dependency is a build
	// failure the branch may have caused.
	closeBlock := func() bool {
		ok := !block || blockMissing || !blockOther
		block, blockMissing, blockOther = false, false, false
		return ok
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case goTestRanAndFailed.MatchString(line):
			return ""
		case strings.HasPrefix(line, "# "):
			if !closeBlock() {
				return ""
			}
			block = true
		case line == "FAIL", strings.HasPrefix(line, "FAIL\t"), strings.HasPrefix(line, "ok "), strings.HasPrefix(line, "?"):
			if !closeBlock() {
				return ""
			}
		case block && strings.TrimSpace(line) != "":
			if dependency := missingBuildDependency(line, tracked); dependency != "" {
				missing = append(missing, dependency)
				blockMissing = true
			} else {
				blockOther = true
			}
		}
	}
	if !closeBlock() {
		return ""
	}
	slices.Sort(missing)
	return strings.Join(slices.Compact(missing), ", ")
}

func missingBuildDependency(line string, tracked func(string) bool) string {
	for _, pattern := range mergeCheckMissingHeader {
		if m := pattern.FindStringSubmatch(line); m != nil {
			if tracked(m[1]) {
				return ""
			}
			return "C header " + m[1]
		}
	}
	for _, d := range mergeCheckMissingDependency {
		if m := d.pattern.FindStringSubmatch(line); m != nil {
			return fmt.Sprintf(d.name, m[1])
		}
	}
	return ""
}

// mergeCheckTrackedFile reports whether the base or the head tracks a file at
// name or ending in /name; trees that cannot be listed count as tracking it.
func mergeCheckTrackedFile(ctx context.Context, dir string, revs ...string) func(string) bool {
	list := sync.OnceValues(func() ([]string, error) {
		var paths []string
		for _, rev := range revs {
			out, err := runGit(ctx, dir, "ls-tree", "-r", "--name-only", "-z", rev)
			if err != nil {
				return nil, err
			}
			paths = append(paths, strings.Split(string(out), "\x00")...)
		}
		return paths, nil
	})
	return func(name string) bool {
		for strings.HasPrefix(name, "./") || strings.HasPrefix(name, "../") {
			_, name, _ = strings.Cut(name, "/")
		}
		paths, err := list()
		return err != nil || slices.ContainsFunc(paths, func(p string) bool { return p == name || strings.HasSuffix(p, "/"+name) })
	}
}
