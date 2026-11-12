//go:build scenario && linux

package scenario

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/update"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// BuildRelease builds b from the repository at repo into out, as Build
// builds the candidate, with b's version stamped where the release
// pipeline stamps it, and writes the build's scenario bundle beside it
// (out + ".bundle"): what the scenario's GitHub publishes next to the
// binary and a scenario build verifies a download against.
func BuildRelease(ctx context.Context, repo, out string, b ReleaseBuild) error {
	stamp := "-X " + project.Module + "/pkg/project.version=" + b.Version
	c := exec.CommandContext(ctx, "go", "build", "-tags", "scenario", "-ldflags", stamp, "-o", out, ".") //nolint:gosec // the repository's own build
	c.Dir = repo
	if raw, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("build release %s: %w\n%s", b.Version, err, raw)
	}
	binary, err := os.ReadFile(out) //nolint:gosec // the build this function wrote
	if err != nil {
		return err
	}
	return os.WriteFile(out+".bundle", update.ScenarioBundle(binary), 0o600)
}

// ReleaseBuilder builds each release a suite's scenarios name once, into
// Dir, and answers every later request with that build.
type ReleaseBuilder struct {
	// Repo is the repository the candidate was built from, Dir where the
	// builds go.
	Repo, Dir string

	mu     sync.Mutex
	builds map[ReleaseBuild]string
}

// Build is the binary of b, built on its first request. Its scenario
// bundle is beside it.
func (rb *ReleaseBuilder) Build(ctx context.Context, b ReleaseBuild) (string, error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if out, ok := rb.builds[b]; ok {
		return out, nil
	}
	dir := filepath.Join(rb.Dir, "release-"+strconv.Itoa(len(rb.builds)))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	out := filepath.Join(dir, ReleaseAsset())
	started := time.Now()
	if err := BuildRelease(ctx, rb.Repo, out, b); err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "scenario release %s built in %s\n", b.Version, time.Since(started).Round(time.Millisecond))
	if rb.builds == nil {
		rb.builds = map[ReleaseBuild]string{}
	}
	rb.builds[b] = out
	return out, nil
}

// placeReleases puts the release builds s names into its root, each with
// its bundle under work/releases/<version>/, and returns the binary the
// desk runs: the installed release's, or the candidate.
func (r *Runner) placeReleases(ctx context.Context, s *Scenario, work string) (string, error) {
	desk := r.Candidate
	for _, b := range s.Given.Releases.builds() {
		if r.Release == nil {
			return "", errors.New("given.releases: this runner builds no releases")
		}
		built, err := r.Release(ctx, b)
		if err != nil {
			return "", err
		}
		dir := filepath.Join(work, "releases", b.Version)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		for _, suffix := range []string{"", ".bundle"} {
			if err := linkOrCopy(built+suffix, filepath.Join(dir, ReleaseAsset()+suffix)); err != nil {
				return "", err
			}
		}
		if b.Version == s.Given.Releases.Installed {
			desk = built
		}
	}
	return desk, nil
}

// tagReleases gives each published release a commit of its own on the
// main of the updater's repository and tags it there, in the order they
// are published. A release is a commit: the lookup takes a tag that shares
// its commit with an older release for a stale one. The scenario's GitHub
// keeps a tag that is there.
func (d *driver) tagReleases(ctx context.Context) error {
	published := d.s.Given.Releases.Published
	if len(published) == 0 {
		return nil
	}
	bare := filepath.Join(originsDir, update.Repository+".git")
	git := func(args ...string) (string, error) {
		c := exec.CommandContext(ctx, binDir+"/git", append([]string{"--git-dir", bare}, args...)...) //nolint:gosec // real git on the scenario's own origin
		c.Env = env(nil)
		out, err := c.Output()
		if err != nil {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	for _, version := range published {
		tree, err := git("rev-parse", "refs/heads/main^{tree}")
		if err != nil {
			return err
		}
		parent, err := git("rev-parse", "refs/heads/main")
		if err != nil {
			return err
		}
		commit, err := git("commit-tree", tree, "-p", parent, "-m", "release "+version)
		if err != nil {
			return err
		}
		for _, ref := range []string{"refs/heads/main", "refs/tags/" + version} {
			if _, err := git("update-ref", ref, commit); err != nil {
				return err
			}
		}
	}
	return nil
}

// binaryOf is the binary a run or background step starts: the desk's, or
// the build of the release the step names.
func binaryOf(r *Run) string {
	if r.Release != "" {
		return releaseBinary(r.Release)
	}
	return candidate
}

// backgroundLog is what the background step name has written so far.
func (d *driver) backgroundLog(name string) (string, error) {
	if _, ok := d.procs[name]; !ok {
		return "", fmt.Errorf("%s was never started", name)
	}
	raw, err := os.ReadFile(filepath.Join(artifactsDir, name+".log")) //nolint:gosec // the step's own log
	return string(raw), err
}
