package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/worktrees"
)

// Every worker builds in a git worktree of its own, and a finished worker's
// worktree stays with its build output (node_modules alone is gigabytes)
// until someone removes it. Every doctor.worktreesEvery a watch looks at the
// linked worktrees under doctor.worktreeRoots: one whose branch's pull
// request merged or closed is removed when it holds nothing but build output,
// and loses only its build output when it holds uncommitted or unpushed work.
// A live session's worktree, a locked one, one with an open or no pull
// request and one touched within worktrees.Grace stay as they are. Each run
// that removed something is logged with the paths and bytes
// (worktrees.trim). One process trims at a time (worktrees.lock), and the
// watches share when it ran last (worktrees.json).

const (
	worktreesFile = "worktrees.json"
	worktreesLock = "worktrees.lock"
	worktreesVerb = "worktrees.trim"
	// worktreesTimeout bounds one run: git in every worktree, the pull
	// requests' reads and the removals.
	worktreesTimeout = 30 * time.Minute
)

// worktreeMachine is what a run reads: the linked worktrees under the roots,
// the folders sessions and processes work in, git and GitHub.
type worktreeMachine struct {
	dirs  []string
	live  []string
	git   worktrees.Git
	pulls func(context.Context, []github.Head) (map[github.Head]github.HeadPull, error)
	now   time.Time
}

// liveWorktrees reads the machine the trim acts on. Tests replace it
// (TestMain), so no test removes the machine's worktrees.
var liveWorktrees = (*app).worktreeMachine

// worktreeMachine reads the worktrees under doctor.worktreeRoots and the
// folders every process, running session and unarchived desktop session
// works in. An unreadable process table fails the run: with no process
// known, every worktree would look unused.
func (a *app) worktreeMachine() (worktreeMachine, error) {
	m := worktreeMachine{
		dirs: worktrees.Find(a.cfg.Doctor.WorktreeRoots),
		git:  runGit,
		pulls: func(ctx context.Context, heads []github.Head) (map[github.Head]github.HeadPull, error) {
			return github.HeadPulls(ctx, github.RunGH, heads)
		},
		now: time.Now(),
	}
	if len(m.dirs) == 0 {
		return m, nil
	}
	t, err := plat.Machine.Processes()
	if err != nil {
		return m, fmt.Errorf("no worktree removed: the process table: %w", err)
	}
	for pid := range t.ByPID {
		if d := proc.Cwd(pid); d != "" {
			m.live = append(m.live, d)
		}
	}
	for _, s := range discover(a.cfg, t, m.now) {
		m.live = append(m.live, s.Cwd)
	}
	for _, r := range claude.ByCLI(a.cfg) {
		if !r.IsArchived {
			m.live = append(m.live, r.Cwd)
		}
	}
	return m, nil
}

// runGit runs git in dir, without its optional locks: a look refreshes no
// index.
func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	c := proc.Command(proc.Background(ctx), "git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args[:min(len(args), 2)], " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// worktreeOutcome is what a run did, or would do, to one worktree.
type worktreeOutcome struct {
	dir   string
	why   string
	bytes int64
	err   error
}

// worktreeRun is one look at the worktrees.
type worktreeRun struct {
	dryRun bool
	// removed are the finished worktrees removed whole, cleaned the
	// finished ones whose build output went and whose work stays, idle the
	// unfinished ones untouched for the grace whose build output went;
	// failed those a removal failed for.
	removed, cleaned, idle, failed []worktreeOutcome
	// kept counts the worktrees left as they are, by kind
	// (worktrees.Kept*).
	kept   map[string]int
	locked bool
	err    error
}

// sum is the bytes of outcomes.
func sum(outcomes ...[]worktreeOutcome) int64 {
	var n int64
	for _, outs := range outcomes {
		for _, o := range outs {
			n += o.bytes
		}
	}
	return n
}

func (r worktreeRun) freed() int64 { return sum(r.removed, r.cleaned, r.idle) }

func (r worktreeRun) acted() bool { return len(r.removed)+len(r.cleaned)+len(r.idle) > 0 }

// String says the run in one line.
func (r worktreeRun) String() string {
	switch {
	case r.locked:
		return "worktrees: another beekeeper trims them now"
	case r.err != nil && !r.acted():
		return fmt.Sprintf("worktrees: %v", r.err)
	}
	verb, of := "removed", "freed"
	if r.dryRun {
		verb, of = "would remove", "would free"
	}
	var parts []string
	tier := func(outs []worktreeOutcome, what string) {
		if len(outs) > 0 {
			parts = append(parts, fmt.Sprintf("%s %s %s", verb, fmt.Sprintf(what, len(outs)), sizeStr(sum(outs))))
		}
	}
	tier(r.removed, "%d finished,")
	tier(r.cleaned, "the build output of %d finished ones holding work,")
	tier(r.idle, "the build output of %d unfinished ones untouched for "+hoursStr(worktrees.Grace)+",")
	if len(parts) == 0 {
		parts = append(parts, "nothing to remove")
	} else {
		parts = append(parts, fmt.Sprintf("%s %s in all", of, sizeStr(r.freed())))
	}
	if n := r.kept[worktrees.KeptWork]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d finished ones kept whole for their work", n))
	}
	if r.dryRun && len(r.kept) > 0 {
		kinds := slices.Sorted(maps.Keys(r.kept))
		counts := make([]string, len(kinds))
		for i, k := range kinds {
			counts[i] = fmt.Sprintf("%d %s", r.kept[k], k)
		}
		parts = append(parts, "kept: "+strings.Join(counts, ", "))
	}
	if len(r.failed) > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", len(r.failed)))
	}
	if r.err != nil {
		parts = append(parts, r.err.Error())
	}
	return "worktrees: " + strings.Join(parts, "; ")
}

// paths lists each worktree the run removed, cleaned or failed on, one per
// line, with its size and why.
func (r worktreeRun) paths() []string {
	var out []string
	add := func(what string, outs []worktreeOutcome) {
		for _, o := range outs {
			l := fmt.Sprintf("  %s %s %s (%s)", what, sizeStr(o.bytes), o.dir, o.why)
			if o.err != nil {
				l += ": " + o.err.Error()
			}
			out = append(out, l)
		}
	}
	add("removed", r.removed)
	add("build output of", r.cleaned)
	add("build output of", r.idle)
	add("failed", r.failed)
	return out
}

// hoursStr says d as whole hours, "6h".
func hoursStr(d time.Duration) string { return fmt.Sprintf("%dh", int(d.Hours())) }

// sizeStr says b in GiB from 1 GiB on, in MiB below.
func sizeStr(b int64) string {
	if b >= 1<<30 {
		return gibStr(b)
	}
	return fmt.Sprintf("%d MiB", b>>20)
}

// notable says the run is worth a doctor's line.
func (r worktreeRun) notable() bool {
	return r.acted() || r.kept[worktrees.KeptWork] > 0 || len(r.failed) > 0 || r.locked || r.err != nil ||
		r.dryRun && len(r.kept) > 0
}

// trimWorktrees looks at the worktrees under the lock on the machine read
// returns and, unless dryRun, removes the finished ones and the build output
// of those that hold work, logging the paths and bytes.
func (a *app) trimWorktrees(dryRun bool, read func() (worktreeMachine, error)) worktreeRun {
	r := worktreeRun{dryRun: dryRun, kept: map[string]int{}}
	lock := flock.New(filepath.Join(a.store.Dir(), worktreesLock))
	ok, err := lock.TryLock()
	if err != nil || !ok {
		r.locked, r.err = err == nil, err
		return r
	}
	defer func() { _ = lock.Unlock() }()
	m, err := read()
	if err != nil {
		r.err = err
		return r
	}
	ctx, cancel := context.WithTimeout(context.Background(), worktreesTimeout)
	defer cancel()
	r.trim(ctx, m)
	if !dryRun && (r.acted() || len(r.failed) > 0) {
		_ = a.store.Log(event(watchParty, worktreesVerb, "%s\n%s", r, strings.Join(r.paths(), "\n")))
	}
	return r
}

// trim judges every worktree of m and acts on the finished ones.
func (r *worktreeRun) trim(ctx context.Context, m worktreeMachine) {
	var trees []worktrees.Tree
	var heads []github.Head
	var errs []error
	for _, d := range m.dirs {
		if under(d, m.live) {
			r.kept[worktrees.KeptLive]++
			continue
		}
		t, err := worktrees.Inspect(ctx, m.git, d)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d, err))
			continue
		}
		trees = append(trees, t)
		if t.Repo != "" && t.Branch != "" {
			heads = append(heads, github.Head{Repo: t.Repo, Branch: t.HeadBranch()})
		}
	}
	var pulls map[github.Head]github.HeadPull
	if len(heads) > 0 {
		var err error
		if pulls, err = m.pulls(ctx, heads); err != nil {
			r.err = errors.Join(append(errs, fmt.Errorf("no worktree removed: the pull requests: %w", err))...)
			return
		}
	}
	for _, t := range trees {
		var pull *github.HeadPull
		if p, ok := pulls[github.Head{Repo: t.Repo, Branch: t.HeadBranch()}]; ok {
			pull = &p
		}
		v := worktrees.Decide(t, pull, false, pull != nil && pushed(ctx, m.git, t, pull.HeadOid), m.now)
		switch v.Action {
		case worktrees.Remove:
			o := worktreeOutcome{dir: t.Dir, why: v.Why, bytes: worktrees.Size(t.Dir)}
			if !r.dryRun {
				o.err = removeWorktree(ctx, m.git, t.Dir)
			}
			r.record(&r.removed, o)
		case worktrees.Clean:
			o := worktreeOutcome{dir: t.Dir, why: v.Why}
			for _, b := range t.Build {
				p := filepath.Join(t.Dir, filepath.FromSlash(strings.TrimSuffix(b, "/")))
				if !strings.HasPrefix(p, t.Dir+string(filepath.Separator)) {
					continue
				}
				o.bytes += worktrees.Size(p)
				if !r.dryRun {
					o.err = errors.Join(o.err, os.RemoveAll(p))
				}
			}
			if v.Kept != "" {
				r.record(&r.idle, o)
			} else {
				r.record(&r.cleaned, o)
			}
		case worktrees.Keep:
			r.kept[v.Kept]++
		}
	}
	r.err = errors.Join(errs...)
}

// record files o under to, or under failed when its action failed.
func (r *worktreeRun) record(to *[]worktreeOutcome, o worktreeOutcome) {
	if o.err != nil {
		r.failed = append(r.failed, o)
		return
	}
	*to = append(*to, o)
}

// under reports whether dir is, or holds, one of the folders.
func under(dir string, folders []string) bool {
	for _, f := range folders {
		if f == dir || strings.HasPrefix(f, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// pushed reports whether t's commits that no remote-tracking branch has are
// its pull request's own: its HEAD is the pull request's head or before it.
func pushed(ctx context.Context, git worktrees.Git, t worktrees.Tree, headOid string) bool {
	if t.Unpushed == 0 || t.Head == headOid {
		return true
	}
	if headOid == "" {
		return false
	}
	_, err := git(ctx, t.Dir, "merge-base", "--is-ancestor", "HEAD", headOid)
	return err == nil
}

// removeWorktree removes the worktree dir through its repository, its build
// output with it, and prunes its record.
func removeWorktree(ctx context.Context, git worktrees.Git, dir string) error {
	out, err := git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	common := strings.TrimSpace(string(out))
	_, err = git(ctx, filepath.Dir(common), "--git-dir="+common, "worktree", "remove", "--force", dir)
	return err
}

// worktreesMark is the watches' shared memory of the trim: when it ran last.
type worktreesMark struct {
	At time.Time `json:"at"`
}

// worktrees runs the trim every doctor.worktreesEvery, in the background so
// the sample goes on. A run that removed something is one WORKTREES line; a
// failed one is WORKTREES FAILED until a run succeeds.
func (w *watcher) worktrees(now time.Time) {
	every := w.cfg.Doctor.WorktreesEvery.Duration
	if !w.worktreesOn || every <= 0 || !w.worktreesTrimming.CompareAndSwap(false, true) {
		return
	}
	if now.Before(w.worktreesNext) {
		w.worktreesTrimming.Store(false)
		return
	}
	d := *w.app
	w.bg.Add(1)
	go func() {
		defer w.bg.Done()
		defer w.worktreesTrimming.Store(false)
		var m worktreesMark
		_, _ = w.store.ReadFile(worktreesFile, &m)
		if now.Sub(m.At) < every {
			w.worktreesNext = m.At.Add(every)
			return
		}
		r := w.trimWorktrees(false, func() (worktreeMachine, error) { return liveWorktrees(&d) })
		if r.locked {
			w.worktreesNext = now.Add(goCacheRetry)
			return
		}
		w.worktreesNext = now.Add(every)
		_ = w.store.WriteFile(worktreesFile, worktreesMark{At: now})
		w.check("worktrees", r.err != nil || len(r.failed) > 0, "WORKTREES FAILED: %s", r)
		if r.acted() {
			w.emitNow("worktrees-trim", "WORKTREES %s (doctor.worktreesEvery)", r)
		}
	}()
}

func (a *app) worktreesCmd() *cobra.Command {
	var (
		apply bool
		roots []string
	)
	c := &cobra.Command{
		Use:   "worktrees [--apply] [--root DIR]...",
		Short: "Remove finished workers' git worktrees and idle worktrees' build output",
		Long: `worktrees looks at the linked git worktrees under doctor.worktreeRoots
(or each --root) and says, with --apply does, what the watch does every
doctor.worktreesEvery, in three tiers:

  finished          the branch's newest pull request merged or closed ` + hoursStr(worktrees.Grace) + `
                    ago (a detached HEAD: untouched ` + hoursStr(worktrees.DetachedIdle) + `) and the
                    worktree holds nothing but build output: removed whole
  finished, work    the same, holding uncommitted changes or unpushed
                    commits: only its build output goes
  idle              any other worktree untouched ` + hoursStr(worktrees.Grace) + ` (an open or no
                    pull request): only its build output goes

Build output is what git ignores and the untracked directories a build
writes again (` + strings.Join(worktrees.Regenerable, ", ") + `); in a kept
worktree only those directories go, git state, branch and commits stay. A
worktree a running process, a running session or an unarchived desktop
session works in, and a locked one, are never touched. Each path is listed
with its size and why; an applied run is logged (worktrees.trim).`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if len(roots) > 0 {
				a.cfg.Doctor.WorktreeRoots = nil
				for _, r := range roots {
					abs, err := filepath.Abs(r)
					if err != nil {
						return usageErr("--root %s: %v", r, err)
					}
					a.cfg.Doctor.WorktreeRoots = append(a.cfg.Doctor.WorktreeRoots, abs)
				}
			}
			r := a.trimWorktrees(!apply, func() (worktreeMachine, error) { return liveWorktrees(a) })
			for _, l := range append([]string{r.String()}, r.paths()...) {
				if _, err := fmt.Fprintln(a.out, l); err != nil {
					return err
				}
			}
			if r.err != nil || len(r.failed) > 0 {
				return fmt.Errorf("worktrees: %d failed: %w", len(r.failed), r.err)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&apply, "apply", false, "remove; without it worktrees only says what it would")
	c.Flags().StringArrayVar(&roots, "root", nil, "look under this folder instead of doctor.worktreeRoots (repeatable)")
	return c
}
