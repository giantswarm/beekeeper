//go:build scenario

package fakegithub

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Origins are the repositories' bare Git origins under Root
// (<owner>/<name>.git): the branches, heads, commits and files GitHub
// serves are the origin's own, read and written with real git.
type Origins struct {
	// Git is the git executable, Root the directory of the origins and
	// Env the environment git runs in.
	Git  string
	Root string
	Env  []string

	// conflicts memoizes Conflicts by repository, base and head: commits
	// never change, and every read of a pull request's mergeable asks.
	conflicts sync.Map
}

// Dir is the origin of a repository.
func (o *Origins) Dir(r *Repo) string {
	return filepath.Join(o.Root, r.Owner, r.Name+".git")
}

// Has says whether the repository has an origin; false for no origins.
func (o *Origins) Has(r *Repo) bool {
	if o == nil || r == nil {
		return false
	}
	fi, err := os.Stat(o.Dir(r))
	return err == nil && fi.IsDir()
}

func (o *Origins) run(r *Repo, env []string, stdin string, args ...string) (string, error) {
	c := exec.Command(o.Git, append([]string{"--git-dir", o.Dir(r)}, args...)...) //nolint:gosec // real git on the scenario's own origin
	c.Env = append(append([]string(nil), o.Env...), env...)
	c.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	c.Stdout, c.Stderr = &out, &errOut
	if err := c.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}
	return out.String(), nil
}

func (o *Origins) git(r *Repo, args ...string) (string, error) {
	out, err := o.run(r, nil, "", args...)
	return strings.TrimSpace(out), err
}

// Resolve resolves a ref or a commit id to a commit id.
func (o *Origins) Resolve(r *Repo, ref string) (string, bool) {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return "", false
	}
	sha, err := o.git(r, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return sha, err == nil && sha != ""
}

// Refs are the short names of the refs under prefix ("refs/heads/"),
// sorted.
func (o *Origins) Refs(r *Repo, prefix string) []string {
	out, err := o.git(r, "for-each-ref", "--format=%(refname)", prefix)
	if err != nil || out == "" {
		return nil
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		names = append(names, strings.TrimPrefix(line, prefix))
	}
	return names
}

// CommitInfo is a commit as the API shows it.
type CommitInfo struct {
	SHA       string
	Tree      string
	Parents   []string
	Author    string
	Email     string
	Authored  time.Time
	Committer string
	Committed time.Time
	Subject   string
	Body      string
}

// Message is the commit's whole message.
func (c CommitInfo) Message() string {
	if c.Body == "" {
		return c.Subject
	}
	return c.Subject + "\n\n" + c.Body
}

// commitFormat separates a commit's fields with the unit separator, which
// no commit message of a scenario holds.
const commitFormat = "%H%x1f%T%x1f%P%x1f%an%x1f%ae%x1f%aI%x1f%cn%x1f%cI%x1f%s%x1f%b"

// CommitInfo reads a commit.
func (o *Origins) CommitInfo(r *Repo, sha string) (CommitInfo, error) {
	out, err := o.run(r, nil, "", "show", "--no-patch", "--format="+commitFormat, sha+"^{commit}")
	if err != nil {
		return CommitInfo{}, err
	}
	return parseCommit(out)
}

func parseCommit(raw string) (CommitInfo, error) {
	f := strings.SplitN(strings.Trim(raw, "\n"), "\x1f", 10)
	if len(f) != 10 {
		return CommitInfo{}, fmt.Errorf("git show: unexpected commit record %q", raw)
	}
	c := CommitInfo{SHA: f[0], Tree: f[1], Author: f[3], Email: f[4], Committer: f[6], Subject: f[8], Body: strings.TrimSpace(f[9])}
	if f[2] != "" {
		c.Parents = strings.Fields(f[2])
	}
	c.Authored, _ = time.Parse(time.RFC3339, f[5])
	c.Committed, _ = time.Parse(time.RFC3339, f[7])
	return c, nil
}

// Log are the commits reachable from head and not from base, oldest
// first, at most limit.
func (o *Origins) Log(r *Repo, base, head string, limit int) ([]CommitInfo, error) {
	spec := head
	if base != "" {
		spec = base + ".." + head
	}
	out, err := o.run(r, nil, "", "log", "--reverse", "--max-count="+strconv.Itoa(limit), "--format="+commitFormat+"%x1e", spec)
	if err != nil {
		return nil, err
	}
	var commits []CommitInfo
	for _, rec := range strings.Split(out, "\x1e") {
		if strings.Trim(rec, "\n") == "" {
			continue
		}
		c, err := parseCommit(rec)
		if err != nil {
			return nil, err
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// AheadBehind counts the commits head has beyond base, and base beyond
// head.
func (o *Origins) AheadBehind(r *Repo, base, head string) (ahead, behind int, err error) {
	out, err := o.git(r, "rev-list", "--left-right", "--count", base+"..."+head)
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("git rev-list --count: %q", out)
	}
	behind, _ = strconv.Atoi(f[0])
	ahead, _ = strconv.Atoi(f[1])
	return ahead, behind, nil
}

// MergeBase is the best common ancestor of two commits.
func (o *Origins) MergeBase(r *Repo, a, b string) (string, error) {
	return o.git(r, "merge-base", a, b)
}

// File reads a file at a commit.
func (o *Origins) File(r *Repo, sha, path string) ([]byte, bool) {
	out, err := o.run(r, nil, "", "show", sha+":"+path)
	if err != nil {
		return nil, false
	}
	return []byte(out), true
}

// BlobSHA is the object id of a file at a commit.
func (o *Origins) BlobSHA(r *Repo, sha, path string) string {
	out, _ := o.git(r, "rev-parse", "--verify", "--quiet", sha+":"+path)
	return out
}

// Diff is the patch of head against its merge base with base.
func (o *Origins) Diff(r *Repo, base, head string) (string, error) {
	out, err := o.run(r, nil, "", "diff", base+"..."+head)
	return out, err
}

// Changed are the files head changes against its merge base with base.
func (o *Origins) Changed(r *Repo, base, head string) ([]PullFile, error) {
	out, err := o.git(r, "diff", "--numstat", base+"..."+head)
	if err != nil {
		return nil, err
	}
	var files []PullFile
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 {
			continue
		}
		add, _ := strconv.Atoi(f[0])
		del, _ := strconv.Atoi(f[1])
		files = append(files, PullFile{Path: f[2], Additions: add, Deletions: del})
	}
	return files, nil
}

// CompareFile is one file of a comparison as GitHub's compare API lists
// it: its status (added, removed, modified, renamed, copied, changed), the
// name it had before a rename or copy, its blob, and its patch, the
// unified diff's hunks without their headers ("" for a binary file).
type CompareFile struct {
	Path      string
	Previous  string
	Status    string
	SHA       string
	Patch     string
	Additions int
	Deletions int
}

// compareStatus maps git's --name-status letters to GitHub's statuses.
var compareStatus = map[byte]string{'A': "added", 'D': "removed", 'M': "modified", 'R': "renamed", 'C': "copied", 'T': "changed"}

// CompareFiles are the files head changes against its merge base with
// base, as GitHub's compare API (base...head) lists them.
func (o *Origins) CompareFiles(r *Repo, base, head string) ([]CompareFile, error) {
	out, err := o.run(r, nil, "", "diff", "--name-status", "-M", "-z", base+"..."+head)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	var files []CompareFile
	for i := 0; i < len(fields); i++ {
		code := fields[i]
		if code == "" {
			continue
		}
		f := CompareFile{Status: compareStatus[code[0]]}
		if f.Status == "" {
			f.Status = "modified"
		}
		paths := []string{}
		if code[0] == 'R' || code[0] == 'C' {
			if i+2 >= len(fields) {
				return nil, fmt.Errorf("git diff --name-status: %q has no paths", code)
			}
			f.Previous, f.Path = fields[i+1], fields[i+2]
			paths = append(paths, f.Previous, f.Path)
			i += 2
		} else {
			if i+1 >= len(fields) {
				return nil, fmt.Errorf("git diff --name-status: %q has no path", code)
			}
			f.Path = fields[i+1]
			paths = append(paths, f.Path)
			i++
		}
		diff, err := o.run(r, nil, "", append([]string{"diff", "--no-color", "--no-ext-diff", "-M", base + "..." + head, "--"}, paths...)...)
		if err != nil {
			return nil, err
		}
		if at := strings.Index(diff, "\n@@"); at >= 0 {
			f.Patch = strings.TrimSuffix(diff[at+1:], "\n")
		} else if strings.HasPrefix(diff, "@@") {
			f.Patch = strings.TrimSuffix(diff, "\n")
		}
		for _, l := range strings.Split(f.Patch, "\n") {
			switch {
			case strings.HasPrefix(l, "+"):
				f.Additions++
			case strings.HasPrefix(l, "-"):
				f.Deletions++
			}
		}
		at := head
		if f.Status == "removed" {
			at = base
		}
		f.SHA = o.BlobSHA(r, at, cmpOr(f.Path, f.Previous))
		files = append(files, f)
	}
	return files, nil
}

// Conflicts reports whether head does not merge into base without
// conflicts: what GitHub's mergeable reports as CONFLICTING.
func (o *Origins) Conflicts(r *Repo, base, head string) (bool, error) {
	key := o.Dir(r) + " " + base + " " + head
	if known, ok := o.conflicts.Load(key); ok {
		return known.(bool), nil
	}
	conflicts, err := o.mergeTreeConflicts(r, base, head)
	if err == nil {
		o.conflicts.Store(key, conflicts)
	}
	return conflicts, err
}

func (o *Origins) mergeTreeConflicts(r *Repo, base, head string) (bool, error) {
	c := exec.Command(o.Git, "--git-dir", o.Dir(r), "merge-tree", "--write-tree", base, head) //nolint:gosec // real git on the scenario's own origin
	c.Env = append([]string(nil), o.Env...)
	err := c.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return true, nil
	}
	return false, err
}

// identity is who the origin's own commits are by.
var identity = []string{
	"GIT_AUTHOR_NAME=GitHub", "GIT_AUTHOR_EMAIL=noreply@github.com",
	"GIT_COMMITTER_NAME=GitHub", "GIT_COMMITTER_EMAIL=noreply@github.com",
}

func (o *Origins) commitTree(r *Repo, tree, message string, parents ...string) (string, error) {
	args := []string{"commit-tree", tree, "-m", message}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	out, err := o.run(r, identity, "", args...)
	return strings.TrimSpace(out), err
}

// Merge merges head into the branch base as GitHub's merge button does:
// a merge commit, one squashed commit, or head's commits replayed as one
// (rebase is served as a squash onto the base, which is what a
// single-commit branch gives). It returns the new head of base.
func (o *Origins) Merge(r *Repo, base, head, method, title, body string) (string, error) {
	baseSHA, ok := o.Resolve(r, "refs/heads/"+base)
	if !ok {
		return "", fmt.Errorf("base branch %s does not exist", base)
	}
	tree, err := o.git(r, "merge-tree", "--write-tree", baseSHA, head)
	if err != nil {
		return "", fmt.Errorf("%s does not merge into %s: %w", head, base, err)
	}
	tree, _, _ = strings.Cut(tree, "\n")
	message := title
	if body != "" {
		message += "\n\n" + body
	}
	parents := []string{baseSHA}
	if method == "merge" {
		parents = append(parents, head)
	}
	sha, err := o.commitTree(r, tree, message, parents...)
	if err != nil {
		return "", err
	}
	if _, err := o.git(r, "update-ref", "refs/heads/"+base, sha, baseSHA); err != nil {
		return "", err
	}
	return sha, nil
}

// UpdateBranch merges base into the branch head (a merge commit) and
// returns head's new commit; with rebase the branch's changes become one
// commit on top of base.
func (o *Origins) UpdateBranch(r *Repo, base, head string, rebase bool) (string, error) {
	baseSHA, ok := o.Resolve(r, "refs/heads/"+base)
	if !ok {
		return "", fmt.Errorf("base branch %s does not exist", base)
	}
	headSHA, ok := o.Resolve(r, "refs/heads/"+head)
	if !ok {
		return "", fmt.Errorf("head branch %s does not exist", head)
	}
	tree, err := o.git(r, "merge-tree", "--write-tree", headSHA, baseSHA)
	if err != nil {
		return "", fmt.Errorf("%s does not merge into %s: %w", base, head, err)
	}
	tree, _, _ = strings.Cut(tree, "\n")
	var sha string
	if rebase {
		info, ierr := o.CommitInfo(r, headSHA)
		if ierr != nil {
			return "", ierr
		}
		sha, err = o.commitTree(r, tree, info.Message(), baseSHA)
	} else {
		sha, err = o.commitTree(r, tree, fmt.Sprintf("Merge branch '%s' into %s", base, head), headSHA, baseSHA)
	}
	if err != nil {
		return "", err
	}
	if _, err := o.git(r, "update-ref", "refs/heads/"+head, sha, headSHA); err != nil {
		return "", err
	}
	return sha, nil
}

// Push commits one file onto a branch and returns the new commit; a new
// branch starts at from.
func (o *Origins) Push(r *Repo, branch, from, path, content, message string) (string, error) {
	parent, ok := o.Resolve(r, "refs/heads/"+branch)
	if !ok {
		if parent, ok = o.Resolve(r, "refs/heads/"+from); !ok {
			return "", fmt.Errorf("branch %s does not exist to start %s from", from, branch)
		}
	}
	index, err := os.CreateTemp("", "fakegithub-index-")
	if err != nil {
		return "", err
	}
	_ = index.Close()
	_ = os.Remove(index.Name())
	defer func() { _ = os.Remove(index.Name()) }()
	env := []string{"GIT_INDEX_FILE=" + index.Name()}
	if _, err := o.run(r, env, "", "read-tree", parent); err != nil {
		return "", err
	}
	blob, err := o.run(r, nil, content, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	if _, err := o.run(r, env, "", "update-index", "--add", "--cacheinfo", "100644,"+strings.TrimSpace(blob)+","+path); err != nil {
		return "", err
	}
	tree, err := o.run(r, env, "", "write-tree")
	if err != nil {
		return "", err
	}
	sha, err := o.commitTree(r, strings.TrimSpace(tree), message, parent)
	if err != nil {
		return "", err
	}
	if _, err := o.git(r, "update-ref", "refs/heads/"+branch, sha); err != nil {
		return "", err
	}
	return sha, nil
}

// SetRef points a ref at a commit.
func (o *Origins) SetRef(r *Repo, ref, sha string) error {
	_, err := o.git(r, "update-ref", ref, sha)
	return err
}

// Heads resolves branches to their commits: a branch that does not exist
// is missing from the map. It reads the origin's ref files (a loose ref,
// else packed-refs) without starting git, since every read of a pull
// request's mergeable asks; git resolves what the files do not answer.
func (o *Origins) Heads(r *Repo, branches ...string) map[string]string {
	heads := map[string]string{}
	dir := o.Dir(r)
	var packed []byte
	for _, b := range branches {
		ref := "refs/heads/" + b
		if raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ref))); err == nil {
			if sha := strings.TrimSpace(string(raw)); isSHA(sha) {
				heads[b] = sha
				continue
			}
		}
		if packed == nil {
			packed, _ = os.ReadFile(filepath.Join(dir, "packed-refs"))
		}
		if sha, ok := packedRef(packed, ref); ok {
			heads[b] = sha
		} else if sha, ok := o.Resolve(r, ref); ok {
			heads[b] = sha
		}
	}
	return heads
}

// packedRef is ref's commit in a packed-refs file.
func packedRef(packed []byte, ref string) (string, bool) {
	for _, line := range strings.Split(string(packed), "\n") {
		if sha, name, ok := strings.Cut(line, " "); ok && name == ref && isSHA(sha) {
			return sha, true
		}
	}
	return "", false
}

// isSHA reports whether s is a full hexadecimal object name.
func isSHA(s string) bool {
	return (len(s) == 40 || len(s) == 64) && strings.Trim(s, "0123456789abcdef") == ""
}

// PullRef is the ref GitHub keeps at pull request n's head.
func PullRef(n int) string { return fmt.Sprintf("refs/pull/%d/head", n) }

// FollowPull points pull request n's ref at branch, so a fetch of
// refs/pull/<n>/head reads the branch's head as it moves, as on GitHub.
func (o *Origins) FollowPull(r *Repo, n int, branch string) error {
	_, err := o.git(r, "symbolic-ref", PullRef(n), "refs/heads/"+branch)
	return err
}

// PinPull keeps pull request n's ref at the commit it names now, for a
// branch about to go: GitHub keeps the ref of a pull request whose branch
// was deleted.
func (o *Origins) PinPull(r *Repo, n int) error {
	sha, ok := o.Resolve(r, PullRef(n))
	if !ok {
		return nil
	}
	_, err := o.git(r, "update-ref", "--no-deref", PullRef(n), sha)
	return err
}

// DeleteRef removes a ref.
func (o *Origins) DeleteRef(r *Repo, ref string) error {
	if _, ok := o.Resolve(r, ref); !ok {
		return &apiError{Status: 422, Message: "Reference does not exist"}
	}
	_, err := o.git(r, "update-ref", "-d", ref)
	return err
}
