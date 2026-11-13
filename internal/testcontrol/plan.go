package testcontrol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/build"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// AllowedTags are the build tags CI runs Go tests with: the scenarios',
// the loop workflow's and the performance suite's. A test that builds only
// with another tag (a live test) or on another platform is reported, not
// run.
var AllowedTags = []string{"scenario", "integration", "perf"}

// How a test of the pull request stands to the merge base.
const (
	// ChangeAdded is a test its package on the base does not have.
	ChangeAdded = "added"
	// ChangeChanged is a test whose code differs from the base's: its
	// doc comment, comments and blank lines do not count.
	ChangeChanged = "changed"
	// ChangeMoved is a test the pull request moves unchanged from another
	// package: it shows nothing new, so the control does not run it.
	ChangeMoved = "moved"
)

// Why the control does not run a test of the pull request.
const (
	SkipNoCode     = "the pull request changes only tests: the merge base with its test files is its own tree"
	SkipOptedOut   = "opted out"
	SkipNewPackage = "new package: the base has none of its code"
	SkipTags       = "builds only with tags or on a platform CI does not test with"
)

// noteBaseOptOut explains a test the control runs although it carries an
// opt-out: the base's own.
const noteBaseOptOut = "its control: none is the base's, written for an earlier change; this change runs"

// FileChange is a file the pull request adds (A), modifies (M) or deletes
// (D); a change of its type is a modification.
type FileChange struct {
	Status byte
	// Path is a slash path relative to the tree's root.
	Path string
}

func (c FileChange) String() string { return string(c.Status) + " " + c.Path }

// Case is one Test function the pull request adds or changes.
type Case struct {
	Name   string `json:"name"`
	File   string `json:"file"`
	Dir    string `json:"dir"`
	Change string `json:"change"`
	// Tags are the build tags it runs with.
	Tags []string `json:"tags,omitempty"`
	// Skip says why the control does not run it; Reason is an opt-out's,
	// Note explains a run.
	Skip   string `json:"skip,omitempty"`
	Reason string `json:"reason,omitempty"`
	Note   string `json:"note,omitempty"`
	// Base is its run on the merge base; Head, for one that fails there,
	// its run on the pull request's code.
	Base string `json:"base,omitempty"`
	Head string `json:"head,omitempty"`
	// Detail is the run's first failures or its build errors.
	Detail string `json:"detail,omitempty"`
}

// Shows says whether the case shows the pull request's change: it fails
// on the base and passes on the pull request's code.
func (c *Case) Shows() bool { return c.Base == Fails && c.Head == Passes }

// Plan is the control of one pull request.
type Plan struct {
	// CodeChanged says whether the pull request changes anything beyond its
	// test files and test data.
	CodeChanged bool
	// Overlay are the test files and test data the pull request changes,
	// the ones the control applies to the base.
	Overlay []FileChange
	// Cases are the tests it adds, changes or moves, in file order.
	Cases []*Case
	// Problems are control lines the control cannot read.
	Problems []string
}

// Run are the cases the control runs on the base.
func (p *Plan) Run() []*Case {
	var out []*Case
	for _, c := range p.Cases {
		if c.Skip == "" {
			out = append(out, c)
		}
	}
	return out
}

// Diff lists the files that differ between the commits base and head of
// the repository at repo.
func Diff(ctx context.Context, repo, base, head string) ([]FileChange, error) {
	out, err := git(ctx, repo, "diff", "--no-renames", "--no-ext-diff", "--name-status", "-z", base, head, "--")
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	if len(fields) == 1 && fields[0] == "" {
		return nil, nil
	}
	if len(fields)%2 != 0 {
		return nil, fmt.Errorf("git diff --name-status -z: odd output %q", out)
	}
	var changes []FileChange
	for i := 0; i < len(fields); i += 2 {
		st := fields[i]
		if st == "" {
			return nil, fmt.Errorf("git diff --name-status -z: no status for %q", fields[i+1])
		}
		c := FileChange{Status: st[0], Path: fields[i+1]}
		if c.Status == 'T' {
			c.Status = 'M'
		}
		if c.Status != 'A' && c.Status != 'M' && c.Status != 'D' {
			return nil, fmt.Errorf("git diff: status %s of %s", st, c.Path)
		}
		changes = append(changes, c)
	}
	return changes, nil
}

// git runs git in dir and returns its output.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, "git", args...) //nolint:gosec // git on the control's own trees
	c.Dir = dir
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// The kinds of a file the pull request changes.
const (
	kindCode     = iota // anything beyond test files and test data
	kindTest            // a _test.go file: applied to the base
	kindTestData        // a file under a testdata directory: applied too
)

// classify sorts a changed file by its path.
func classify(p string) int {
	switch {
	case slices.Contains(strings.Split(path.Dir(p), "/"), "testdata"):
		return kindTestData
	case strings.HasSuffix(path.Base(p), "_test.go"):
		return kindTest
	default:
		return kindCode
	}
}

// PlanControl plans the control of the pull request whose files changes
// lists: head is its tree, base the merge base's, before the control
// applies the pull request's test files to it.
//
// A test of the pull request is the base's of the same file and name, or
// else of the same directory and name; one that is the base's unchanged is
// no case. An opt-out holds for the change that writes it: a test whose
// control line is the base's runs when its code changes.
func PlanControl(changes []FileChange, head, base string) (*Plan, error) {
	p := &Plan{}
	var headFiles, baseFiles []string
	for _, c := range changes {
		switch classify(c.Path) {
		case kindTest:
			p.Overlay = append(p.Overlay, c)
			if c.Status != 'D' {
				headFiles = append(headFiles, c.Path)
			}
			if c.Status != 'A' {
				baseFiles = append(baseFiles, c.Path)
			}
		case kindTestData:
			p.Overlay = append(p.Overlay, c)
		case kindCode:
			p.CodeChanged = true
		}
	}
	// Where a changed or moved test comes from: the base's versions of the
	// test files the pull request changes or deletes.
	byFile, byDir, byName := map[string]*Test{}, map[string]*Test{}, map[string][]*Test{}
	for _, f := range baseFiles {
		tests, err := readTests(base, f)
		if err != nil {
			return nil, fmt.Errorf("the merge base's %w", err)
		}
		for _, t := range tests {
			byFile[t.File+"\x00"+t.Name] = t
			if _, ok := byDir[t.Dir+"\x00"+t.Name]; !ok {
				byDir[t.Dir+"\x00"+t.Name] = t
			}
			byName[t.Name] = append(byName[t.Name], t)
		}
	}
	for _, f := range headFiles {
		tests, err := readTests(head, f)
		if err != nil {
			return nil, err
		}
		for _, t := range tests {
			old := byFile[t.File+"\x00"+t.Name]
			if old == nil {
				old = byDir[t.Dir+"\x00"+t.Name]
			}
			if old != nil && old.Code == t.Code {
				continue
			}
			c := &Case{Name: t.Name, File: t.File, Dir: t.Dir, Change: ChangeAdded}
			p.Cases = append(p.Cases, c)
			if old != nil {
				c.Change = ChangeChanged
			} else if i := slices.IndexFunc(byName[t.Name], func(o *Test) bool { return o.Code == t.Code }); i >= 0 {
				c.Change, c.Skip = ChangeMoved, "moved unchanged from "+byName[t.Name][i].Dir
				continue
			}
			p.decide(c, t, old, head, base)
		}
	}
	return p, nil
}

// decide decides whether the control runs an added or changed test, and
// with which tags.
func (p *Plan) decide(c *Case, t, old *Test, head, base string) {
	optedOut := false
	if t.Control != "" {
		reason, err := optOut(t.Control)
		switch {
		case err != nil:
			p.Problems = append(p.Problems, fmt.Sprintf("%s: %s: %v", t.File, t.Name, err))
		case old != nil && old.Control == t.Control:
			c.Note = noteBaseOptOut
		default:
			c.Reason, optedOut = reason, true
		}
	}
	tags, ok := tagsFor(head, t.File)
	c.Tags = tags
	switch {
	case !p.CodeChanged:
		c.Skip = SkipNoCode
	case optedOut:
		c.Skip = SkipOptedOut
	case hasCode(head, t.Dir) && !hasCode(base, t.Dir):
		c.Skip = SkipNewPackage
	case !ok:
		c.Skip = SkipTags
	}
}

// readTests reads the Test functions of the file rel of the tree root.
func readTests(root, rel string) ([]*Test, error) {
	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) //nolint:gosec // a test file of the tree
	if err != nil {
		return nil, err
	}
	tests, err := testsOf(rel, src)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	return tests, nil
}

// hasCode says whether the directory dir of the tree root holds a non-test
// Go file: a package the test can run against.
func hasCode(root, dir string) bool {
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
	if err != nil {
		return false
	}
	return slices.ContainsFunc(entries, func(e fs.DirEntry) bool {
		n := e.Name()
		return !e.IsDir() && strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go")
	})
}

// tagsFor returns the fewest of AllowedTags with which go test builds the
// file rel of the tree root on this platform.
func tagsFor(root, rel string) ([]string, bool) {
	dir := filepath.Join(root, filepath.FromSlash(path.Dir(rel)))
	for _, tags := range subsets(AllowedTags) {
		ctx := build.Default
		ctx.BuildTags = tags
		if ok, err := ctx.MatchFile(dir, path.Base(rel)); err == nil && ok {
			return tags, true
		}
	}
	return nil, false
}

// subsets are the subsets of tags, the smaller first.
func subsets(tags []string) [][]string {
	var out [][]string
	for n := 0; n <= len(tags); n++ {
		for mask := range 1 << len(tags) {
			var set []string
			for i, t := range tags {
				if mask&(1<<i) != 0 {
					set = append(set, t)
				}
			}
			if len(set) == n {
				out = append(out, set)
			}
		}
	}
	return out
}

// Apply applies the pull request's test files and test data to the base:
// what it adds or modifies is copied from head, what it deletes is
// removed.
func Apply(overlay []FileChange, head, base string) error {
	for _, c := range overlay {
		dst := filepath.Join(base, filepath.FromSlash(c.Path))
		if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if c.Status == 'D' {
			continue
		}
		src := filepath.Join(head, filepath.FromSlash(c.Path))
		fi, err := os.Lstat(src)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(src)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, dst); err != nil {
				return err
			}
			continue
		}
		raw, err := os.ReadFile(src) //nolint:gosec // a test file of the pull request
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, raw, fi.Mode().Perm()); err != nil { //nolint:gosec // the pull request's own mode
			return err
		}
	}
	return nil
}
