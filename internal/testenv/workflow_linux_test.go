//go:build linux

//nolint:goconst // Literal CLI and API fixtures keep the external protocol visible.
package testenv_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/testenv"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "--sandbox" {
		if err := testenv.EnterSandbox(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := syscall.Exec("/work/bin/fixture", append([]string{"fixture"}, os.Args[3:]...), testenv.WorkflowEnv()); err != nil { //nolint:gosec // the test binary and fixture commands
			panic(err)
		}
	}
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "--") {
		var err error
		switch os.Args[1] {
		case "--linger":
			err = linger()
		case "--heartbeat":
			for {
				if err = write("/work/heartbeat", time.Now().String()); err != nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		case "--scenario":
			err = scenario()
		case "--git-remote":
			err = gitRemote(os.Args[2:])
		case "--gh":
			err = adapter(os.Args[2:])
		case "--claude":
			err = claudeFixture(os.Args[2:])
		case "--turn":
			err = turn(os.Args[2], os.Args[3])
		default:
			err = fmt.Errorf("unknown fixture mode %q", os.Args[1])
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Each parallel run owns its root and PID namespace. Even a killed driver can
// only leave scratch files: no process, mount, or shared cleanup survives it.
func TestSupervisorWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real candidate")
	}
	if out, err := exec.Command("unshare", "-Urmpnf", "--kill-child", "true").CombinedOutput(); err != nil { //nolint:gosec // Fixed fixture executable/arguments or the candidate in this run's private workspace.
		t.Skipf("Linux user/mount/PID/network namespaces required: %v: %s", err, out)
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	for _, dir := range []string{"bin", "repo", "home"} {
		mustMkdir(t, filepath.Join(work, dir))
	}
	candidate := filepath.Join(work, "bin/beekeeper")
	build := exec.Command("go", "build", "-o", candidate, "../..") //nolint:gosec // Fixed fixture executable/arguments or the candidate in this run's private workspace.
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build candidate: %v\n%s", err, out)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self) //nolint:gosec // Test-owned scratch files or this test executable.
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(work, "bin/fixture"), data, 0700); err != nil { //nolint:gosec // Test-owned scratch files or this test executable.
		t.Fatal(err)
	}
	for _, name := range []string{"gh", "claude", "git-remote-https"} {
		if err = os.WriteFile(filepath.Join(work, "bin", name), []byte("#!/bin/sh\nexec /work/bin/fixture --"+strings.ReplaceAll(name, "git-remote-https", "git-remote")+" \"$@\"\n"), 0700); err != nil { //nolint:gosec // Test-owned scratch files or this test executable.
			t.Fatal(err)
		}
	}
	// Two simultaneous copies catch accidental shared files and registrations.
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runRoot := t.TempDir()
			if err := os.CopyFS(filepath.Join(runRoot, "work"), os.DirFS(work)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			c := exec.CommandContext(ctx, "unshare", "-Urmpnf", "--kill-child", self, "--sandbox", runRoot, "--scenario") //nolint:gosec // Fixed fixture executable/arguments or the candidate in this run's private workspace.
			c.Env = []string{"PATH=" + os.Getenv("PATH"), "GH_TOKEN=unavailable-fixture-credential", "SSH_AUTH_SOCK=/unavailable-fixture-socket"}
			if out, err := c.CombinedOutput(); err != nil {
				t.Fatalf("workflow: %v\n%s", err, out)
			}
		})
	}
	t.Run("interrupted", func(t *testing.T) {
		t.Parallel()
		runRoot := t.TempDir()
		if err := os.CopyFS(filepath.Join(runRoot, "work"), os.DirFS(work)); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(t.TempDir(), "other-run")
		if err := os.WriteFile(sentinel, []byte("untouched"), 0600); err != nil { //nolint:gosec // Test-owned scratch files or this test executable.
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		c := exec.CommandContext(ctx, "unshare", "-Urmpnf", "--kill-child", self, "--sandbox", runRoot, "--linger") //nolint:gosec // Fixed fixture executable/arguments or the candidate in this run's private workspace.
		c.Env = []string{"PATH=" + os.Getenv("PATH")}
		var output bytes.Buffer
		c.Stdout = &output
		c.Stderr = &output
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cancel(); _ = c.Wait() })
		heartbeat := filepath.Join(runRoot, "work/heartbeat")
		if err := until(func() (bool, error) { b, _ := os.ReadFile(heartbeat); return len(b) > 0, nil }); err != nil { //nolint:gosec // Test-owned scratch files or this test executable.
			t.Fatalf("heartbeat: %v", err)
		}
		cancel()
		if err := c.Wait(); err == nil {
			t.Fatal("interrupted run succeeded")
		}
		before, err := os.ReadFile(heartbeat) //nolint:gosec // Test-owned scratch files or this test executable.
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		after, err := os.ReadFile(heartbeat) //nolint:gosec // Test-owned scratch files or this test executable.
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("descendant survived namespace teardown")
		}
		if b, err := os.ReadFile(sentinel); err != nil || string(b) != "untouched" { //nolint:gosec // Test-owned scratch files or this test executable.
			t.Fatal("another run was touched")
		}
	})
}

func linger() error {
	c := exec.Command("/work/bin/fixture", "--heartbeat") //nolint:gosec // Fixed fixture executable/arguments or the candidate in this run's private workspace.
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0700); err != nil {
		t.Fatal(err)
	}
}
func command(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // Fixed fixture executable/arguments or the candidate in this run's private workspace.
	out, err := c.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %v: %w\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}
func bk(args ...string) (string, error)  { return command("beekeeper", args...) }
func git(args ...string) (string, error) { return command("git", args...) }
func write(p, s string) error            { return os.WriteFile(p, []byte(s), 0600) } //nolint:gosec // Test-owned scratch files or this test executable.
func readState() (*state.State, error) {
	s, err := state.Open("/work/state")
	if err != nil {
		return nil, err
	}
	return s.Read()
}
func until(f func() (bool, error)) error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ok, err := f()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for durable fixture state")
}

func scenario() (err error) {
	defer func() {
		if err != nil {
			b, _ := os.ReadFile("/work/api.json") //nolint:gosec // Test-owned scratch files or this test executable.
			fmt.Fprintln(os.Stderr, string(b))
			paths, _ := filepath.Glob("/work/turn-*.log")
			for _, p := range paths {
				b, _ := os.ReadFile(p) //nolint:gosec // Test-owned scratch files or this test executable.
				fmt.Fprintf(os.Stderr, "%s: %s\n", filepath.Base(p), b)
			}
		}
	}()
	// Verify isolation before executing any production command.
	conn, e := net.DialTimeout("tcp", "192.0.2.1:443", 100*time.Millisecond)
	if e == nil {
		conn.Close()
		return fmt.Errorf("outbound network is available")
	}

	if _, err := os.Stat("/home"); !os.IsNotExist(err) {
		return fmt.Errorf("host homes exposed")
	}
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "SSH_AUTH_SOCK", "DBUS_SESSION_BUS_ADDRESS"} {
		if os.Getenv(key) != "" {
			return fmt.Errorf("credential environment exposed: %s", key)
		}
	}
	if err := write("/work/config.yaml", `stateDir: /work/state
leaseDir: /work/leases
identity: {person: fixture, team: fixture, host: fixture}
github: {floor: -1, allowedOrgs: [o]}
guide: {person: fixture}
agents:
  mode: background
  dir: /work/repo
  staleAfter: -1s
capacity:
  ceiling: 2
  reviewHeadroom: 0
  availMinMiB: 1
  providers: {claude: {ceiling: 2}}
  choices: [{harness: claude, model: sonnet}]
watch: {availMinMiB: 1, loadMax: 1000000}
supervisor:
  effort: low
  workerBrief: /work/worker.txt
  ownership: {authors: [desk]}
board:
  owner: o
  project: 1
  tidyEvery: -1s
  escalateEvery: -1s
  order: [{name: ready, status: [Ready]}]
merge: {methods: {o/r: merge}}
disk: {checkouts: [/work/repo]}
`); err != nil {
		return err
	}
	if err := write("/work/worker.txt", "Work {{.URL}}"); err != nil {
		return err
	}
	for _, args := range [][]string{{"init", "--bare", "/work/origin.git"}, {"init", "-b", "main"}, {"remote", "add", "origin", "https://github.com/o/r.git"}} {
		if _, err := git(args...); err != nil {
			return err
		}
	}
	if err := write("base.txt", "base\n"); err != nil {
		return err
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "base"}, {"push", "origin", "main"}} {
		if _, err := git(args...); err != nil {
			return err
		}
	}
	base, err := git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if err = saveAPI(apiState{Status: "Ready", Base: base}); err != nil {
		return err
	}
	store, e := state.Open("/work/state")
	if e != nil {
		return e
	}
	if e = store.Update(func(s *state.State) ([]state.Event, error) {
		s.AutoInstallPaused = true
		s.Budget = &state.Budget{At: time.Now().UTC(), Remaining: 5000, Limit: 5000, Reset: time.Now().Add(time.Hour)}
		return nil, nil
	}); e != nil {
		return e
	}
	if e = store.Record(state.Event{At: time.Now().UTC(), By: state.Party{Name: "watch"}, Verb: "watch.budget-state", Detail: "false"}); e != nil {
		return e
	}
	out, err := bk("supervisor", "loop", "--once")
	fmt.Println(out)
	if err != nil {
		return err
	}
	if err = until(func() (bool, error) {
		s, e := readState()
		if e != nil {
			return false, e
		}
		return len(s.Starts) > 0 && len(s.Agents) > 0, nil
	}); err != nil {
		return err
	}
	if err = until(func() (bool, error) { a, e := loadAPI(); return a.Parked, e }); err != nil {
		return err
	}

	// Start the independent reviewer, then prove an approval of an old commit
	// cannot advance the base through the production parser, planner, or gate.
	if out, err = bk("supervisor", "loop", "--once"); err != nil {
		return err
	}
	fmt.Println(out)
	if err = until(func() (bool, error) { a, e := loadAPI(); return len(a.Reviews) == 1, e }); err != nil {
		return err
	}
	if out, err = bk("supervisor", "loop", "--once"); err != nil {
		return err
	}
	fmt.Println(out)
	current, err := git("--git-dir=/work/origin.git", "rev-parse", "refs/heads/main")
	if err != nil {
		return err
	}
	staleAPI, e := loadAPI()
	if e != nil {
		return e
	}
	if slices.ContainsFunc(staleAPI.Calls, func(args []string) bool { return len(args) > 1 && args[0] == "pr" && args[1] == "merge" }) {
		return fmt.Errorf("stale approval reached the merge call")
	}
	if current != base {
		return fmt.Errorf("stale approval advanced the base")
	}
	if err = write("/work/allow-review", "approve the current head"); err != nil {
		return err
	}
	if err = until(func() (bool, error) { _, e := os.Stat("/work/reviewer-done"); return e == nil, nil }); err != nil {
		return err
	}
	for i := 0; i < 4; i++ {
		if out, err = bk("supervisor", "loop", "--once"); err != nil {
			return err
		}
		fmt.Println(out)
		a, e := loadAPI()
		if e != nil {
			return e
		}
		if a.Merged {
			break
		}
	}

	a, err := loadAPI()
	if err != nil {
		return err
	}
	if !a.Merged || len(a.Reviews) != 2 || a.Reviews[1]["commit_id"] != a.Head {
		return fmt.Errorf("not merged with independent review: head=%s reviews=%d unknown=%s", a.Head, len(a.Reviews), a.Unknown)
	}
	actual, err := git("--git-dir=/work/origin.git", "rev-parse", "refs/heads/main")
	if err != nil {
		return err
	}
	if actual == base || actual != a.MergeCommit {
		return fmt.Errorf("base did not advance to recorded merge: %s %s", actual, a.MergeCommit)
	}
	if _, err = git("--git-dir=/work/origin.git", "merge-base", "--is-ancestor", a.Head, actual); err != nil {
		return err
	}
	content, e := git("--git-dir=/work/origin.git", "show", actual+":change.txt")
	if e != nil {
		return e
	}
	if content != "deterministic worker" {
		return fmt.Errorf("merged tree lacks worker change")
	}
	out, err = bk("supervisor", "loop", "--once")
	fmt.Println(out)
	if err != nil {
		return err
	}
	s, err := readState()
	if err != nil {
		return err
	}
	if len(s.Starts) != 2 || !s.Starts[0].Background || !s.Starts[1].Background || s.Starts[0].Session == s.Starts[1].Session {
		return fmt.Errorf("missing independent durable starts")
	}
	if !slices.ContainsFunc(s.WorkerReports, func(r state.WorkerReport) bool {
		return r.Finished && r.By.Session == s.Starts[1].Session && strings.Contains(r.Text, "Head reviewed: `"+a.Head+"`")
	}) {
		return fmt.Errorf("missing independent exact-head reviewer completion")
	}

	if slices.ContainsFunc(s.Agents, func(a state.Agent) bool { return a.Task != "" }) {
		return fmt.Errorf("task not durably completed: %+v", s.Agents)
	}
	events, err := store.Events(0, nil)
	if err != nil {
		return err
	}
	for _, verb := range []string{"agents.start", "supervisor.review", "supervisor.merge", "supervisor.end", "merging", "merged"} {
		if !slices.ContainsFunc(events, func(e state.Event) bool { return e.Verb == verb }) {
			return fmt.Errorf("missing durable %s", verb)
		}
	}
	if a.Unknown != "" {
		return fmt.Errorf("unknown external call: %s", a.Unknown)
	}
	if _, err = command("gh", "api", "unexpected-endpoint"); err == nil {
		return fmt.Errorf("unknown external call succeeded")
	}
	rejected, err := loadAPI()
	if err != nil {
		return err
	}
	if rejected.Unknown != "api unexpected-endpoint" {
		return fmt.Errorf("unknown call was not retained")
	}
	return nil
}
