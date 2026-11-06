package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

const (
	retireOperation = "retire"

	// RetireRequested, RetirePreserved and RetireRetired are the stages of the
	// one retirement every terminal path enters. A stage is durable before the
	// next begins, and each step is safe to repeat. RetireNoJournal answers a
	// retirement of an execution its host holds no journal of: nothing
	// journal-backed is left, which is not a claim about unowned files.
	RetireRequested = "requested"
	RetirePreserved = "preserved"
	RetireRetired   = "retired"
	RetireNoJournal = "no-journal"

	// The triggers that enter a retirement. They differ in why, never in how.
	RetireTriggerReport      = "report"
	RetireTriggerStop        = "stop"
	RetireTriggerFailedStart = "failed-start"
	RetireTriggerCrash       = "crash"

	// The resource kinds of an execution's inventory.
	ResourceExecutionRoot  = "execution-root"
	ResourceWorktree       = "worktree"
	ResourceRepository     = "repository"
	ResourceBranch         = "branch"
	ResourceUpstreamBranch = "upstream-branch"
	ResourceScratch        = "scratch"
	ResourceTemporary      = "temporary"
	ResourceCache          = "cache"
	ResourceToolchain      = "toolchain"
	ResourceSourceBundle   = "source-bundle"
	ResourceCredential     = "credential-handle" //nolint:gosec // a resource kind, not a credential
	ResourceMCPHome        = "mcp-home"

	// DefaultRetention is how long moved-aside resources are kept when the host
	// configures no retention, the same week the desk gives its aside entries.
	DefaultRetention = 7 * 24 * time.Hour

	retiredWorkspaces = ".retired"
	retiredResources  = "retired"
	recoveryBundles   = "recovery"
	workerBranch      = "beekeeper-worker/"
	recoveryRefs      = "refs/beekeeper/recovery/"
	sourceOwnerFile   = "owner.json"
	retireIndexFile   = ".retire-index"
	// maxAbandonedCommits bounds the commits read from a worker's reflogs; the
	// moved-aside repository keeps any beyond it.
	maxAbandonedCommits = "256"
	temporaryDir        = "tmp"
	cacheDir            = "cache"
	toolchainDir        = "go"
)

// ErrUnfinished refuses the retirement of an execution that may still run or
// resume. A parked worker between turns is one: it keeps its worktree and thread.
var ErrUnfinished = errors.New("execution is unfinished: its owned unit is not confirmed ended")

// Resource is one thing an execution owns on its host, recorded against the
// execution before it exists. Aside is where retirement moves it; a resource
// without one travels with its execution root or, for an upstream branch, stays
// as publication evidence. Journals, logs and control receipts are evidence,
// not resources: they stay until the host's retention says otherwise.
type Resource struct {
	Owner Ref    `json:"owner"`
	Kind  string `json:"kind"`
	Path  string `json:"path,omitempty"`
	Ref   string `json:"ref,omitempty"`
	Aside string `json:"aside,omitempty"`
}

// Recovery is the durable reference to work a retirement preserved. Commit is
// one commit reaching the worker's head, its branch, what only its reflogs still
// reach and a snapshot of its dirty and untracked files. Bundle holds it past
// Base; Repository is the moved-aside repository, which also keeps Ref and every
// other ref of the worker.
type Recovery struct {
	Ref          string `json:"ref"`
	Commit       string `json:"commit"`
	Base         string `json:"base"`
	Head         string `json:"head"`
	Branch       string `json:"branch,omitempty"`
	Dirty        bool   `json:"dirty,omitempty"`
	Bundle       string `json:"bundle"`
	BundleDigest string `json:"bundleDigest"`
	Repository   string `json:"repository"`
}

// RetireRequest asks the host to retire a terminal execution. Repeating it,
// under any ID or trigger, resumes the retirement already recorded.
type RetireRequest struct {
	ID      string `json:"id"`
	Trigger string `json:"trigger"`
	Reason  string `json:"reason"`
}

// Retirement is the host's durable state of one retirement.
type Retirement struct {
	Ref         Ref       `json:"ref"`
	ID          string    `json:"id"`
	Trigger     string    `json:"trigger"`
	Reason      string    `json:"reason"`
	Stage       string    `json:"stage"`
	Recovery    *Recovery `json:"recovery,omitempty"`
	RequestedAt time.Time `json:"requestedAt"`
	RetiredAt   time.Time `json:"retiredAt,omitzero"`
	RetainUntil time.Time `json:"retainUntil,omitzero"`
	LastError   string    `json:"lastError,omitempty"`
}

// Finished says the host holds nothing further to retire for this execution.
func (r Retirement) Finished() bool { return r.Stage == RetireRetired || r.Stage == RetireNoJournal }

// Retirer is the optional host operation behind a coordinator's retirement.
type Retirer interface {
	Retire(context.Context, Ref, RetireRequest) (Retirement, error)
}

func (t SSHTransport) Retire(ctx context.Context, r Ref, q RetireRequest) (Retirement, error) {
	var x Retirement
	err := t.Call(ctx, retireOperation, r, q, &x)
	if err == nil && (x.Ref != r || !x.Finished()) {
		err = errors.New("retirement is not acknowledged")
	}
	return x, err
}

func (q RetireRequest) validate() error {
	if q.ID == "" || len(q.ID) > 128 || q.Reason == "" || len(q.Reason) > 1024 {
		return errors.New("retirement needs an idempotency key and reason")
	}
	switch q.Trigger {
	case RetireTriggerReport, RetireTriggerStop, RetireTriggerFailedStart, RetireTriggerCrash:
		return nil
	}
	return errors.New("invalid retirement trigger")
}

func retiredLabel(r Ref) string { return fmt.Sprintf("%s-%d", r.Execution, r.Generation) }

// executionDir is the owned root of everything a worker writes: its worktree,
// repository, scratch, temporary files, caches and toolchain downloads.
func (s *Server) executionDir(r Ref) string {
	return filepath.Join(s.Config.WorkspaceRoot, string(r.Execution))
}

func (s *Server) recoveryBundle(r Ref) string {
	return filepath.Join(s.Root, recoveryBundles, retiredLabel(r)+".bundle")
}

// ownedResources is the inventory of r, derived only from its identity. That
// derivation is the ownership check too: a journal naming anything else names
// something this execution does not own.
func (s *Server) ownedResources(r Ref) []Resource {
	id := string(r.Execution)
	dir := s.executionDir(r)
	aside := filepath.Join(s.Root, retiredResources, retiredLabel(r))
	branch := "refs/heads/" + workerBranch + id
	return []Resource{
		{Owner: r, Kind: ResourceExecutionRoot, Path: dir, Aside: filepath.Join(s.Config.WorkspaceRoot, retiredWorkspaces, retiredLabel(r))},
		{Owner: r, Kind: ResourceWorktree, Path: filepath.Join(dir, "workspace")},
		{Owner: r, Kind: ResourceRepository, Path: filepath.Join(dir, "repository.git")},
		{Owner: r, Kind: ResourceBranch, Ref: branch},
		{Owner: r, Kind: ResourceUpstreamBranch, Ref: "refs/heads/" + GitHubBranch(r)},
		{Owner: r, Kind: ResourceScratch, Path: filepath.Join(dir, "workspace", ".beekeeper-scratch")},
		{Owner: r, Kind: ResourceTemporary, Path: filepath.Join(dir, temporaryDir)},
		{Owner: r, Kind: ResourceCache, Path: filepath.Join(dir, cacheDir)},
		{Owner: r, Kind: ResourceToolchain, Path: filepath.Join(dir, toolchainDir)},
		{Owner: r, Kind: ResourceSourceBundle, Path: filepath.Dir(s.sourcePath(r)), Aside: filepath.Join(aside, "sources")},
		{Owner: r, Kind: ResourceCredential, Path: filepath.Join(s.Root, workerGitHubBinDir, id), Aside: filepath.Join(aside, workerGitHubBinDir)},
		{Owner: r, Kind: ResourceMCPHome, Path: s.agyMCPHomeDir(r), Aside: filepath.Join(aside, "agy-home")},
	}
}

// ownedEnvironment routes everything a worker writes outside its worktree into
// its owned root, so a worker's temporary files, caches and downloaded Go
// toolchains retire with it. Provisioned tools stay where they are installed.
func (s *Server) ownedEnvironment(j Journal, env []string) ([]string, error) {
	dir := s.executionDir(j.Handle.Ref)
	temporary, cache, gopath := filepath.Join(dir, temporaryDir), filepath.Join(dir, cacheDir), filepath.Join(dir, toolchainDir)
	for _, p := range []string{temporary, cache, gopath} {
		if err := ownedDirectory(p); err != nil {
			return nil, err
		}
	}
	routed := map[string]string{
		"TMPDIR": temporary, "TMP": temporary, "TEMP": temporary, "GOTMPDIR": temporary,
		"XDG_CACHE_HOME": cache, "GOCACHE": filepath.Join(cache, "go-build"),
		"GOPATH": gopath, "GOMODCACHE": filepath.Join(gopath, "pkg", "mod"),
	}
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		_, ok := routed[k]
		return ok
	})
	for _, k := range slices.Sorted(maps.Keys(routed)) {
		out = append(out, k+"="+routed[k])
	}
	return out, nil
}

// Retire takes a terminal execution through its one retirement: work is
// preserved durably, then its owned resources move aside, nothing is deleted.
// Report, stop, failed start and crash all arrive here; a retry resumes at the
// recorded stage, so an interrupted retirement never starts over or is skipped.
// An error always leaves the retirement unfinished.
func (s *Server) Retire(ctx context.Context, r Ref, q RetireRequest) (Retirement, error) {
	if err := q.validate(); err != nil {
		return Retirement{}, err
	}
	l, p, err := s.lock(ctx, r)
	if err != nil {
		return Retirement{}, err
	}
	defer func() { _ = l.Unlock() }()
	j, err := s.Read(r)
	if errors.Is(err, os.ErrNotExist) {
		return s.retireUnjournaled(ctx, r)
	}
	if err != nil {
		return Retirement{}, err
	}
	if j.Retirement != nil && j.Retirement.Finished() {
		return *j.Retirement, nil
	}
	if err := s.retirable(ctx, j); err != nil {
		return Retirement{}, err
	}
	// A journal older than the inventory records its resources here, durably,
	// before anything acts on them.
	if len(j.Resources) == 0 {
		j.Resources = s.ownedResources(r)
	}
	if !sameResources(j.Resources, s.ownedResources(r)) {
		return Retirement{}, errors.New("execution inventory names resources this execution does not own")
	}
	if j.Retirement == nil {
		j.Retirement = &Retirement{Ref: r, ID: q.ID, Trigger: q.Trigger, Reason: q.Reason, Stage: RetireRequested, RequestedAt: s.now()}
	}
	if err := atomicFile(p, mustJSON(j)); err != nil {
		return Retirement{}, err
	}
	if err := s.advanceRetirement(ctx, &j, p); err != nil {
		j.Retirement.LastError = err.Error()
		return *j.Retirement, errors.Join(err, atomicFile(p, mustJSON(j)))
	}
	return *j.Retirement, nil
}

func (s *Server) advanceRetirement(ctx context.Context, j *Journal, p string) error {
	if j.Retirement.Stage == RetireRequested {
		recovery, err := s.preserveWork(ctx, *j)
		if err != nil {
			return err
		}
		j.Retirement.Recovery, j.Retirement.Stage, j.Retirement.LastError = recovery, RetirePreserved, ""
		if err := atomicFile(p, mustJSON(j)); err != nil {
			return err
		}
	}
	for _, res := range j.Resources {
		if res.Aside == "" {
			continue
		}
		if err := s.moveAside(res); err != nil {
			return err
		}
	}
	now := s.now()
	j.Retirement.Stage, j.Retirement.RetiredAt, j.Retirement.RetainUntil, j.Retirement.LastError = RetireRetired, now, now.Add(s.Config.retention()), ""
	return atomicFile(p, mustJSON(j))
}

// retirable requires a terminal execution whose owned unit is confirmed ended.
// Anything else, an unknown supervisor state included, stays unfinished.
func (s *Server) retirable(ctx context.Context, j Journal) error {
	if j.ActivationAttempted {
		if s.Launcher == nil {
			return ErrUnfinished
		}
		if state := s.Launcher.State(ctx, j.Handle.Unit); state != unitInactive && state != Failed {
			return ErrUnfinished
		}
		return nil
	}
	if j.StopAcknowledged || j.Phase == Failed {
		return nil
	}
	return ErrUnfinished
}

func sameResources(a, b []Resource) bool {
	sorted := func(in []Resource) []Resource {
		out := slices.Clone(in)
		slices.SortFunc(out, func(x, y Resource) int { return strings.Compare(x.Kind+x.Path+x.Ref, y.Kind+y.Path+y.Ref) })
		return out
	}
	return slices.Equal(sorted(a), sorted(b))
}

// preserveWork records a recovery reference for whatever the worker left that
// its starting commit does not hold: commits, edits and untracked files. A worker
// that never launched left none, and a clean tree on its starting commit needs
// none. Work that cannot be read fails the retirement instead of being assumed
// clean; a repeat after an interruption starts from the same tree.
func (s *Server) preserveWork(ctx context.Context, j Journal) (*Recovery, error) {
	workspace := j.Handle.Workspace
	if !j.ActivationAttempted || workspace == "" || j.Handle.Commit == "" {
		return nil, nil
	}
	if _, err := os.Lstat(workspace); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	dir := s.executionDir(j.Handle.Ref)
	if workspace != filepath.Join(dir, "workspace") {
		return nil, errors.New("workspace identity mismatch")
	}
	if resolved, err := filepath.EvalSymlinks(workspace); err != nil || resolved != workspace {
		return nil, errors.New("workspace resolves through a symlink; it is preserved in place")
	}
	repository := filepath.Join(dir, "repository.git")
	if resolved, err := filepath.EvalSymlinks(repository); err != nil || resolved != repository {
		return nil, errors.New("execution repository resolves through a symlink; it is preserved in place")
	}
	inert, err := s.inertGit(ctx, repository)
	if err != nil {
		return nil, err
	}
	git := func(env []string, args ...string) (string, error) {
		out, err := s.gitEnv(ctx, workspace, env, append(slices.Clone(inert), args...)...)
		return strings.TrimSpace(string(out)), err
	}
	// The worker controls its .git file and repository configuration: only a
	// worktree that still resolves to its own metadata is read.
	if common, err := git(nil, "rev-parse", "--path-format=absolute", "--git-common-dir"); err != nil || common != repository {
		return nil, errors.New("workspace no longer uses its own repository; it is preserved in place")
	}
	if top, err := git(nil, "rev-parse", "--show-toplevel"); err != nil || top != workspace {
		return nil, errors.New("workspace no longer is its repository's worktree; it is preserved in place")
	}
	head, err := git(nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, errors.New("cannot read the worker's head; its work is preserved in place")
	}
	branch, err := git(nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+workerBranch+string(j.Handle.Ref.Execution))
	if err != nil {
		branch = ""
	}
	status, err := git(nil, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return nil, errors.New("cannot read the worker's status; its work is preserved in place")
	}
	// A worker that reset or switched away leaves commits only its reflogs
	// reach: a tree back on its starting commit is not proof of no work.
	reachable := []string{"rev-list", "--max-count=" + maxAbandonedCommits, "--reflog", "--not", head, j.Handle.Commit}
	if branch != "" {
		reachable = append(reachable, branch)
	}
	abandoned, err := git(nil, reachable...)
	if err != nil {
		return nil, errors.New("cannot read the worker's reflogs; its work is preserved in place")
	}
	if head == j.Handle.Commit && branch == j.Handle.Commit && status == "" && abandoned == "" {
		return nil, nil
	}
	var abandonedTips []string
	if abandoned != "" {
		tips, err := git(nil, append([]string{"merge-base", "--independent"}, strings.Fields(abandoned)...)...)
		if err != nil {
			return nil, errors.New("cannot read the worker's abandoned commits; its work is preserved in place")
		}
		abandonedTips = strings.Fields(tips)
	}
	index := filepath.Join(dir, retireIndexFile)
	if err := os.Remove(index); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	defer func() { _ = os.Remove(index) }()
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := git(env, "read-tree", head); err != nil {
		return nil, errors.New("cannot snapshot the worker's tree; its work is preserved in place")
	}
	if _, err := git(env, "add", "-A", "--", "."); err != nil {
		return nil, errors.New("cannot snapshot the worker's files; its work is preserved in place")
	}
	tree, err := git(env, "write-tree")
	if err != nil {
		return nil, errors.New("cannot snapshot the worker's tree; its work is preserved in place")
	}
	args := []string{"-c", "user.name=beekeeper", "-c", "user.email=beekeeper@execution.invalid", "commit-tree", tree, "-p", head}
	if branch != "" && branch != head {
		args = append(args, "-p", branch)
	}
	for _, tip := range abandonedTips {
		args = append(args, "-p", tip)
	}
	commit, err := git(nil, append(args, "-m", "recovery of execution "+string(j.Handle.Ref.Execution))...)
	if err != nil {
		return nil, errors.New("cannot record the recovery commit; the work is preserved in place")
	}
	ref := fmt.Sprintf("%s%s/%d", recoveryRefs, j.Handle.Ref.Execution, j.Handle.Ref.Generation)
	if _, err := git(nil, "update-ref", ref, commit); err != nil {
		return nil, errors.New("cannot record the recovery reference; the work is preserved in place")
	}
	bundle, digest, err := s.writeRecoveryBundle(ctx, j.Handle.Ref, repository, ref, j.Handle.Commit)
	if err != nil {
		return nil, err
	}
	aside := filepath.Join(s.Config.WorkspaceRoot, retiredWorkspaces, retiredLabel(j.Handle.Ref), "repository.git")
	return &Recovery{Ref: ref, Commit: commit, Base: j.Handle.Commit, Head: head, Branch: branch, Dirty: status != "", Bundle: bundle, BundleDigest: digest, Repository: aside}, nil
}

// writeRecoveryBundle publishes a verified bundle: a reader sees a whole one
// or none. A stale temporary of an interrupted attempt is this execution's own.
func (s *Server) writeRecoveryBundle(ctx context.Context, r Ref, repository, ref, base string) (string, string, error) {
	path := s.recoveryBundle(r)
	if err := ownedDirectory(filepath.Dir(path)); err != nil {
		return "", "", err
	}
	temporary := path + ".partial.bundle"
	for _, stale := range []string{temporary, temporary + ".lock"} {
		if err := os.Remove(stale); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", "", err
		}
	}
	defer func() { _ = os.Remove(temporary) }()
	if _, err := s.git(ctx, repository, "bundle", "create", "--quiet", temporary, ref, "^"+base); err != nil {
		return "", "", errors.New("cannot write the recovery bundle; the work is preserved in place")
	}
	if _, err := s.git(ctx, repository, "bundle", "verify", "--quiet", temporary); err != nil {
		return "", "", errors.New("recovery bundle does not verify; the work is preserved in place")
	}
	digest, err := fileDigest(temporary)
	if err != nil {
		return "", "", err
	}
	if err := os.Rename(temporary, path); err != nil {
		return "", "", err
	}
	return path, digest, syncDirectory(filepath.Dir(path))
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(filepath.Clean(path)) //nolint:gosec // bundle this retirement just wrote in the owned recovery directory
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// inertGit neutralizes the filter drivers of a repository the worker could
// configure: an ended worker's clean filter must not run as the host's
// retirement. A name git could not be told to override fails closed.
func (s *Server) inertGit(ctx context.Context, repository string) ([]string, error) {
	out, err := s.git(ctx, repository, "config", "--name-only", "--get-regexp", `^filter\.`)
	if err != nil {
		// git config exits 1 for no match; a broken configuration fails the
		// reads that follow.
		return nil, nil
	}
	var args []string
	for name := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if name == "" {
			continue
		}
		if strings.Contains(name, "=") {
			return nil, errors.New("execution repository has an unsafe filter name; it is preserved in place")
		}
		value := ""
		if strings.HasSuffix(name, ".required") {
			value = "false"
		}
		args = append(args, "-c", name+"="+value)
	}
	return args, nil
}

// protectedPaths are what no execution owns: provisioned sources, harness
// homes, the installed tools and the component itself.
func (s *Server) protectedPaths() []string {
	paths := []string{s.codexHomeDir(), s.agyHomeDir()}
	if s.Executable != "" {
		paths = append(paths, filepath.Dir(s.Executable))
	}
	for _, source := range s.Config.Repositories {
		paths = append(paths, source)
		if resolved, err := filepath.EvalSymlinks(source); err == nil {
			paths = append(paths, resolved)
		}
	}
	for _, h := range s.Config.Harnesses {
		if h.ConfigDir != "" {
			paths = append(paths, h.ConfigDir)
		}
	}
	for _, t := range s.Config.Tools {
		paths = append(paths, filepath.Dir(t.Executable))
	}
	return paths
}

func strictlyWithin(root, target string) bool {
	return filepath.Clean(root) != filepath.Clean(target) && within(root, target)
}

// moveAside renames one owned directory under its retired root. A rename
// neither follows nor copies what is inside it, so a symlink a worker left in its
// tree cannot lead it elsewhere; the resource itself and the directories above it
// must be real. Repeating it after an interruption finds the move done.
func (s *Server) moveAside(res Resource) error {
	root := s.Root
	if res.Kind == ResourceExecutionRoot {
		root = s.Config.WorkspaceRoot
	}
	if resolved, err := filepath.EvalSymlinks(root); err != nil || resolved != filepath.Clean(root) {
		return errors.New("retirement root is missing or resolves through a symlink")
	}
	if !strictlyWithin(root, res.Path) || !strictlyWithin(root, res.Aside) {
		return errors.New("owned resource escapes its root")
	}
	for _, p := range s.protectedPaths() {
		if within(p, res.Path) || within(res.Path, p) {
			return errors.New("owned resource overlaps a provisioned path")
		}
	}
	parent := filepath.Dir(res.Path)
	if resolved, err := filepath.EvalSymlinks(parent); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil || resolved != parent {
		return errors.New("owned resource resolves through a symlink")
	}
	info, err := os.Lstat(res.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("owned %s is not a real directory; it is left in place", res.Kind)
	}
	if _, err := os.Lstat(res.Aside); err == nil {
		return fmt.Errorf("owned %s and its retired copy both exist; both are left in place", res.Kind)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ownedDirectory(filepath.Dir(res.Aside)); err != nil {
		return err
	}
	if err := os.Rename(res.Path, res.Aside); err != nil {
		return err
	}
	return errors.Join(syncDirectory(parent), syncDirectory(filepath.Dir(res.Aside)))
}

// syncDirectory makes a rename durable before the journal says it happened.
func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(filepath.Clean(path)) //nolint:gosec // owned retirement directory
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// retireUnjournaled retires what a start left before any journal existed: a
// source bundle uploaded for a reservation its host never acknowledged. Its
// owner record is the inventory; a directory without a matching one is of
// unknown ownership and stays. The caller holds the execution's lock.
func (s *Server) retireUnjournaled(ctx context.Context, r Ref) (Retirement, error) {
	if s.Launcher == nil {
		return Retirement{}, ErrUnfinished
	}
	if state := s.Launcher.State(ctx, UnitName(r)); state != unitInactive && state != Failed {
		return Retirement{}, ErrUnfinished
	}
	done := Retirement{Ref: r, Stage: RetireNoJournal, RetiredAt: s.now(), RetainUntil: s.now().Add(s.Config.retention())}
	dir := filepath.Dir(s.sourcePath(r))
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return done, nil
	}
	if err != nil {
		return Retirement{}, err
	}
	if !info.IsDir() {
		return Retirement{}, errors.New("source directory is not a real directory; it is left in place")
	}
	var owner Ref
	if err := readJSON(filepath.Join(dir, sourceOwnerFile), &owner); err != nil || owner != r {
		return Retirement{}, errors.New("source directory has no matching owner record; it is left in place")
	}
	for _, res := range s.ownedResources(r) {
		if res.Kind == ResourceSourceBundle {
			return done, s.moveAside(res)
		}
	}
	return done, nil
}
