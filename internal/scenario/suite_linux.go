//go:build scenario && linux

package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	fakes "github.com/giantswarm/beekeeper/internal/scenario/harness"
	"github.com/giantswarm/beekeeper/internal/testenv"
)

// SandboxFlag is the test binary's first argument when it runs as a
// scenario's driver inside its sandbox, followed by the scenario's root.
const SandboxFlag = "--scenario-sandbox"

// EnterAndDrive is the driver's entry: it enters the sandbox of root (its
// own user, mount, PID and network namespaces, made by unshare) and runs
// the scenario there. It returns the process's exit code.
func EnterAndDrive(root string) int {
	if err := testenv.EnterSandbox(root); err != nil {
		fmt.Fprintln(os.Stderr, "scenario sandbox:", err)
		return 1
	}
	if err := Drive(); err != nil {
		fmt.Fprintln(os.Stderr, "scenario driver:", err)
		return 1
	}
	return 0
}

// Namespaces checks that this host gives an unprivileged process its own
// user, mount, PID and network namespaces: without them the harness
// refuses to run rather than run against the host.
func Namespaces() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "unshare", "-Urmpnf", "--kill-child", "true").CombinedOutput()
	if err != nil {
		return fmt.Errorf("refusing to run scenarios without Linux user, mount, PID and network namespaces (unshare -Urmpnf): %v: %s", err, out)
	}
	return nil
}

// Build builds the candidate from the repository at repo with the
// scenario build tag into out.
func Build(ctx context.Context, repo, out string) error {
	c := exec.CommandContext(ctx, "go", "build", "-tags", "scenario", "-o", out, ".") //nolint:gosec // the repository's own build
	c.Dir = repo
	if b, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("build candidate: %w\n%s", err, b)
	}
	return nil
}

// Runner runs scenarios, each in a sandbox of its own.
type Runner struct {
	// Candidate is the built beekeeper, Fake this test binary (the fakes
	// and the driver).
	Candidate string
	Fake      string
	// Artifacts, when set, receives each scenario's artifacts under
	// <flow>/<name>/.
	Artifacts string
}

// Run runs one scenario in root, a fresh empty directory, and returns its
// result; Outcome decides whether it fails the run.
func (r *Runner) Run(ctx context.Context, s *Scenario, root string) *Result {
	started := time.Now()
	res := r.run(ctx, s, root)
	res.Name, res.Flow, res.File, res.Pending = s.Name, s.Flow, s.File, s.Pending
	if res.Duration == 0 {
		res.Duration = time.Since(started)
	}
	if r.Artifacts != "" {
		dst := filepath.Join(r.Artifacts, s.Flow, s.Name)
		if err := copyTree(filepath.Join(root, "work", "artifacts"), dst); err != nil && res.Harness == "" {
			res.Harness = "artifacts: " + err.Error()
		}
		if raw, err := json.MarshalIndent(res, "", "  "); err == nil {
			_ = os.WriteFile(filepath.Join(dst, "result.json"), raw, 0o600)
		}
	}
	return res
}

func (r *Runner) run(ctx context.Context, s *Scenario, root string) *Result {
	harness := func(err error) *Result { return &Result{Harness: err.Error()} }
	work := filepath.Join(root, "work")
	for _, dir := range []string{"bin", "home", "repo", "artifacts"} {
		if err := os.MkdirAll(filepath.Join(work, dir), 0o700); err != nil {
			return harness(err)
		}
	}
	bin := filepath.Join(work, "bin")
	if err := linkOrCopy(r.Candidate, filepath.Join(bin, "beekeeper")); err != nil {
		return harness(err)
	}
	if err := linkOrCopy(r.Fake, filepath.Join(bin, fakeBinary)); err != nil {
		return harness(err)
	}
	for _, name := range s.programs() {
		if err := os.Symlink(fakeBinary, filepath.Join(bin, name)); err != nil {
			return harness(err)
		}
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return harness(fmt.Errorf("real git: %w", err))
	}
	if git, err = filepath.EvalSymlinks(git); err != nil {
		return harness(err)
	}
	// git is the fake too: the real git with GitHub's URLs led to the
	// scenario's origins by a remote helper of the fake's (harness.RealGit).
	execPath, err := exec.CommandContext(ctx, git, "--exec-path").Output() //nolint:gosec // the real git
	if err != nil {
		return harness(fmt.Errorf("real git's exec path: %w", err))
	}
	if err := os.MkdirAll(filepath.Join(bin, "git-core"), 0o700); err != nil {
		return harness(err)
	}
	links := map[string]string{"git": fakeBinary, "real-git": git, "real-git-core": strings.TrimSpace(string(execPath)), "sh": "/bin/sh"}
	for _, helper := range fakes.RemoteHelpers {
		links[filepath.Join("git-core", helper)] = filepath.Join("..", fakeBinary)
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(bin, name)); err != nil {
			return harness(err)
		}
	}
	raw, err := os.ReadFile(s.File)
	if err != nil {
		return harness(err)
	}
	if err := os.WriteFile(filepath.Join(work, "scenario.yaml"), raw, 0o600); err != nil {
		return harness(err)
	}
	// The scenario bounds itself; this bounds a driver that does not.
	ctx, cancel := context.WithTimeout(ctx, s.Timeout.Duration+30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "unshare", "-Urmpnf", "--kill-child", r.Fake, SandboxFlag, root) //nolint:gosec // this test binary as the scenario's driver
	// The driver's own environment holds nothing of the host's but PATH;
	// what it starts gets the scenario's allowlist.
	c.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := c.CombinedOutput()
	_ = os.WriteFile(filepath.Join(work, "artifacts", "driver.log"), out, 0o600)
	res := &Result{}
	b, rerr := os.ReadFile(filepath.Join(work, "result.json"))
	if rerr != nil {
		return harness(fmt.Errorf("driver left no result (%v): %v\n%s", err, rerr, out))
	}
	if err := json.Unmarshal(b, res); err != nil {
		return harness(fmt.Errorf("driver result: %w", err))
	}
	return res
}

// linkOrCopy hard-links src to dst, or copies it executable.
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src) //nolint:gosec // the candidate or this test binary
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700) //nolint:gosec // the scenario's own bin directory
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// copyTree copies the regular files under src into dst.
func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // the scenario's artifacts
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600)
	})
}
