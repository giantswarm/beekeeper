//go:build unix

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// fakeBinary is a gate call's binary whose path another file was renamed
// over (replaced) and whose re-exec fails with execErr, recording the
// environment each attempt carried: the call carries on in this process.
type fakeBinary struct {
	replaced bool
	execErr  error
	execs    [][]string
}

func (f *fakeBinary) Path() string   { return "/opt/beekeeper" }
func (f *fakeBinary) Replaced() bool { return f.replaced }
func (f *fakeBinary) Exec(env ...string) error {
	f.execs = append(f.execs, env)
	return f.execErr
}

var errBusy = errors.New("text file busy")

// capture redirects *f (os.Stdout or os.Stderr) until the returned func is
// called, which gives what was written.
func capture(t *testing.T, f **os.File) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	was := *f
	*f = w
	t.Cleanup(func() { *f = was })
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	return func() string {
		*f = was
		_ = w.Close()
		return <-done
	}
}

// deadPID is the pid of a process that has ended.
func deadPID(t *testing.T) int {
	t.Helper()
	c := exec.Command("true")
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	return c.Process.Pid
}

// launchedMerge is o/r#7's merge running devctl (fakeDevctl's script) in a
// merge-child of the gate's, its stderr captured, once devctl has printed
// its first line.
func launchedMerge(t *testing.T, script string) (g *gateRun, base string, pid int, stderr func() string) {
	t.Helper()
	noSystemd(t)
	fakeDevctl(t, script)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s1")
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_NAME", ownerName)
	g = runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}})
	base, err := g.mergeFiles()
	if err != nil {
		t.Fatal(err)
	}
	if pid, err = launchChild(childSpec{Argv: mergeArgv(scratchRepo), Owner: g.me}, base); err != nil {
		t.Fatal(err)
	}
	g.started(pid)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if info, err := os.Stat(base + ".log"); err == nil && info.Size() > 0 {
			break
		}
	}
	return g, base, pid, capture(t, &os.Stderr)
}

const devctlFirstLine = "merging\n"

// A gate following its devctl when its binary is replaced (an install)
// re-executes the installed one, which follows the same devctl on from
// where the stderr was copied and records the outcome: the merge's lane
// entry leaves with the real outcome, nothing is copied twice, and the
// re-exec that does not start is followed on under the old binary.
func TestAReplacedGateFollowsItsDevctlOnUnderTheInstalledOne(t *testing.T) {
	script := `echo merging >&2; sleep 1; echo "waiting for the release" >&2; echo '` + mergedDoc + `'`
	t.Run("the installed binary follows on", func(t *testing.T) {
		g, base, pid, stderr := launchedMerge(t, script)
		stubGitHub(t, "", "")
		run := followChild(base, pid, 0, &fakeBinary{replaced: true}, nil)
		if !run.replaced || run.offset != int64(len(devctlFirstLine)) {
			t.Fatalf("replaced %v at offset %d, want %d", run.replaced, run.offset, len(devctlFirstLine))
		}
		for _, ext := range []string{".log", ".pid"} {
			if _, err := os.Stat(base + ext); err != nil {
				t.Errorf("the run's %s file is gone before the re-exec: %v", ext, err)
			}
		}
		// The re-executed gate: the same process, arguments and stdio.
		t.Setenv(gateRunningEnv, fmt.Sprintf("%d:%d", pid, run.offset))
		stdout := capture(t, &os.Stdout)
		err := g.gate(context.Background(), mergeArgv(scratchRepo), time.Minute, true)
		doc, said := stdout(), stderr()
		if err != nil {
			t.Fatalf("exit %d (%v), want devctl's 0\n%s", Code(err), err, said)
		}
		if strings.TrimSpace(doc) != mergedDoc {
			t.Errorf("stdout %q, want the document", doc)
		}
		if strings.Count(said, "merging") != 1 || !strings.Contains(said, "waiting for the release") ||
			!strings.Contains(said, "continuing under beekeeper") || !strings.Contains(said, fmt.Sprintf("following o/r#7's devctl (pid %d) on", pid)) {
			t.Errorf("stderr:\n%s", said)
		}
		if _, ok := os.LookupEnv(gateRunningEnv); ok {
			t.Error("the running marker is left in the environment, devctl would inherit it")
		}
		if d := lastEvent(t, g, "merged"); !strings.Contains(d, "o/r#7 exit 0, release v1.2.4") {
			t.Errorf("merged event: %q", d)
		}
		if st := gateState(t, g); len(st.Merges) != 0 {
			t.Errorf("merges left: %+v", st.Merges)
		}
		if _, err := os.Stat(base + ".rc"); err == nil {
			t.Error("the run's files are left")
		}
	})
	t.Run("a re-exec that does not start follows on", func(t *testing.T) {
		g, base, pid, stderr := launchedMerge(t, script)
		stubGitHub(t, "", "")
		bin := &fakeBinary{replaced: true, execErr: errBusy}
		g.bin = bin
		stdout := capture(t, &os.Stdout)
		err := g.follow(base, pid, 0)
		doc, said := stdout(), stderr()
		if err != nil {
			t.Fatalf("exit %d (%v), want devctl's 0\n%s", Code(err), err, said)
		}
		want := fmt.Sprintf("%s=%d:%d", gateRunningEnv, pid, len(devctlFirstLine))
		if len(bin.execs) != 1 || !slices.Contains(bin.execs[0], want) {
			t.Errorf("re-exec attempts %q, want one with %s", bin.execs, want)
		}
		if strings.TrimSpace(doc) != mergedDoc {
			t.Errorf("stdout %q, want the document", doc)
		}
		if strings.Count(said, "merging") != 1 || !strings.Contains(said, "waiting for the release") ||
			!strings.Contains(said, "/opt/beekeeper was replaced while o/r#7's devctl ran: re-executing it") ||
			!strings.Contains(said, "the new binary does not start (text file busy): carrying on under") {
			t.Errorf("stderr:\n%s", said)
		}
		if d := lastEvent(t, g, "merged"); !strings.Contains(d, "o/r#7 exit 0, release v1.2.4") {
			t.Errorf("merged event: %q", d)
		}
		if st := gateState(t, g); len(st.Merges) != 0 {
			t.Errorf("merges left: %+v", st.Merges)
		}
	})
}

// A gate whose binary file is replaced while its devctl runs (an install
// renaming a new beekeeper over its path) re-executes the new file and exits
// with devctl's own code: 0 for a merge, and a refusal's code unchanged, never
// the re-executed process's own. The gate is this test binary run as
// beekeeper from a copy that the test renames another copy over.
func TestAGateReplacedMidMergeExitsWithDevctlsCode(t *testing.T) {
	if runningBinary().Path() == "" {
		t.Skip("the running binary cannot be read here")
	}
	for _, c := range []struct {
		name string
		doc  string
		rc   int
	}{
		{verbMerged, mergedDoc, 0},
		{"refused", `{"verdict":"red","reason":"check lint concluded failure"}`, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			noSystemd(t)
			dir := t.TempDir()
			proceed := filepath.Join(dir, "proceed")
			fakeDevctl(t, fmt.Sprintf(`echo merging >&2; while [ ! -e %s ]; do sleep 0.05; done; echo "waiting for the release" >&2; echo '%s'; exit %d`,
				proceed, c.doc, c.rc))
			stateHome := filepath.Join(dir, "state")
			store, err := state.Open(filepath.Join(stateHome, "beekeeper"))
			if err != nil {
				t.Fatal(err)
			}
			base := mergeBase(store.Dir(), scratchRepo, 7)
			if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
				t.Fatal(err)
			}
			pid, err := launchChild(childSpec{Argv: mergeArgv(scratchRepo)}, base)
			if err != nil {
				t.Fatal(err)
			}
			self := filepath.Join(dir, "bin", "beekeeper")
			copyTestBinary(t, self)
			gate := exec.Command(self, append([]string{gateCmdName, "--"}, mergeArgv(scratchRepo)...)...) //nolint:gosec // this test binary's copy
			gate.Env = append(os.Environ(), "BEEKEEPER_TEST_MAIN=1", "BEEKEEPER_CONFIG="+filepath.Join(dir, "none.yaml"), "XDG_STATE_HOME="+stateHome,
				"CLAUDE_CODE_SESSION_ID=", "CLAUDE_CODE_HOST_SESSION_ID=", fmt.Sprintf("%s=%d:0", gateRunningEnv, pid))
			var stdout strings.Builder
			gate.Stdout = &stdout
			stderr := &lines{}
			gate.Stderr = stderr
			if err := gate.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- gate.Wait() }()
			t.Cleanup(func() {
				_ = os.WriteFile(proceed, nil, 0o600)
				_ = gate.Process.Kill()
			})
			err = store.Update(func(st *state.State) ([]state.Event, error) {
				st.Merges = []state.Merge{{Repo: scratchRepo, PR: 7, Lane: scratchRepo, PID: gate.Process.Pid, Phase: state.Running, Joined: relayNow, Started: relayNow}}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			stderr.await(t, "following o/r#7's devctl", 1)
			replacement := self + ".new"
			copyTestBinary(t, replacement)
			if err := os.Rename(replacement, self); err != nil {
				t.Fatal(err)
			}
			stderr.await(t, "was replaced while o/r#7's devctl ran: re-executing it", 1)
			stderr.await(t, "following o/r#7's devctl", 2)
			if err := os.WriteFile(proceed, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			select {
			case <-exited:
			case <-time.After(30 * time.Second):
				t.Fatalf("the gate did not end\n%s", stderr)
			}
			said := stderr.String()
			if rc := gate.ProcessState.ExitCode(); rc != c.rc {
				t.Fatalf("the gate exited %d, want devctl's %d\n%s", rc, c.rc, said)
			}
			if strings.TrimSpace(stdout.String()) != c.doc {
				t.Errorf("stdout %q, want devctl's document", stdout.String())
			}
			if strings.Count(said, "merging") != 1 || !strings.Contains(said, "waiting for the release") {
				t.Errorf("stderr:\n%s", said)
			}
			verb := verbMerged
			if c.rc != 0 {
				verb = "merge.failed"
			}
			evs, err := store.Events(0, func(e state.Event) bool { return e.Verb == verb })
			if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Detail, fmt.Sprintf("o/r#7 exit %d", c.rc)) {
				t.Errorf("%s events %+v (%v)", verb, evs, err)
			}
		})
	}
}

// copyTestBinary copies this test binary, which doubles as beekeeper, to path.
func copyTestBinary(t *testing.T, path string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(self) //nolint:gosec // this test binary
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o700); err != nil { //nolint:gosec // an executable copy
		t.Fatal(err)
	}
}

// lines is a process's output, safe to read while it writes.
type lines struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// await waits up to 30 s for the output to say s n times.
func (l *lines) await(t *testing.T, s string, n int) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if strings.Count(l.String(), s) >= n {
			return
		}
	}
	t.Fatalf("the gate did not say %q %d times:\n%s", s, n, l)
}

// A gate whose devctl ran on a release older than the one that wrote the
// state meanwhile (an install whose re-exec did not reach it) cannot record
// the outcome: it leaves the run's document and exit code, and the watch
// records the real outcome from them at its next poll, without asking
// GitHub, so no running entry stays for a finished merge.
func TestAStaleGatesOutcomeIsRecordedByTheWatchFromItsFiles(t *testing.T) {
	noSystemd(t)
	fakeDevctl(t, `echo merged >&2; echo '`+mergedDoc+`'`)
	asked := stubGitHub(t, github.Open, "")
	store, err := state.OpenVersion(t.TempDir(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	g := runningMergeIn(t, store, scratchRepo, config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}})
	// The gate's process is gone once its devctl ended, as the watch sees it.
	g.pid = deadPID(t)
	newer, err := state.OpenVersion(store.Dir(), newerRelease)
	if err != nil {
		t.Fatal(err)
	}
	err = newer.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges[0].PID = g.pid
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if g.store, err = state.OpenVersion(store.Dir(), olderRelease); err != nil {
		t.Fatal(err)
	}
	stderr := capture(t, &os.Stderr)
	stdout := capture(t, &os.Stdout)
	err = g.runMerge()
	doc, said := stdout(), stderr()
	if err != nil {
		t.Fatalf("exit %d (%v), want devctl's 0\n%s", Code(err), err, said)
	}
	if strings.TrimSpace(doc) != mergedDoc {
		t.Errorf("stdout %q, want the document", doc)
	}
	if !strings.Contains(said, "o/r#7's outcome is not recorded by this call, which runs beekeeper v0.1.0, older than the v0.2.0 that wrote the state") ||
		!strings.Contains(said, "which records the run at its next poll") {
		t.Errorf("stderr:\n%s", said)
	}
	base := mergeBase(store.Dir(), scratchRepo, 7)
	if raw, err := os.ReadFile(base + ".rc"); err != nil || strings.TrimSpace(string(raw)) != "0" { //nolint:gosec // the test's file
		t.Errorf("the run's exit code is not left for the watch: %q, %v", raw, err)
	}
	if raw, err := os.ReadFile(base + ".json"); err != nil || strings.TrimSpace(string(raw)) != mergedDoc { //nolint:gosec // the test's file
		t.Errorf("the run's document is not left for the watch: %q, %v", raw, err)
	}
	st, err := newer.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Writer == nil || st.Writer.Version != newerRelease || len(st.Merges) != 1 || st.Merges[0].Phase != state.Running {
		t.Fatalf("writer %+v, merges %+v: the older release's gate touched the state", st.Writer, st.Merges)
	}
	if d := lastEvent(t, g, state.VerbStaleWriter); !strings.Contains(d, "its save is refused") {
		t.Errorf("stale-writer event %q", d)
	}

	w := &app{cfg: g.cfg, store: newer, now: time.Now()}
	recorded, lost := w.recordGone(context.Background())
	if len(recorded) != 1 || !strings.Contains(recorded[0], "o/r#7 exit 0, release v1.2.4") || len(lost) != 0 {
		t.Fatalf("recorded %q, lost %+v", recorded, lost)
	}
	if *asked != 0 {
		t.Errorf("GitHub asked %d times about a run whose document is in its files", *asked)
	}
	if st, _ = newer.Read(); len(st.Merges) != 0 {
		t.Errorf("merges left: %+v", st.Merges)
	}
	if _, err := os.Stat(base + ".rc"); err == nil {
		t.Error("the run's files are left after the record")
	}
}

// A merge-child whose binary is replaced while its command runs re-executes
// the installed one with the outcome, which hands it to the owner running
// nothing again; a re-exec that does not start hands it over under the old
// binary.
func TestAReplacedMergeChildHandsItsOutcomeOverUnderTheInstalledOne(t *testing.T) {
	owner := state.Party{Session: "s1", Name: ownerName}
	a, _, woke := tellOwnerRun(t, owner)
	base := filepath.Join(a.store.Dir(), "merges", "o_r-7")
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := childSpec{Argv: []string{"/bin/sh", "-c", "echo '" + greenDoc + "'; echo 'checks pending' >&2; exit 4"},
		Command: strings.Fields("devctl pr wait o/r 7"), Owner: owner, Gate: deadPID(t)}
	raw, _ := json.Marshal(spec)
	if err := os.WriteFile(base+".spec", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := &fakeBinary{replaced: true, execErr: errBusy}
	stderr := capture(t, &os.Stderr)
	err := a.mergeChildRun(context.Background(), base, bin)
	said := stderr()
	if err != nil {
		t.Fatalf("unit exit %d (%v), want 0 for devctl's 4\n%s", Code(err), err, said)
	}
	if len(bin.execs) != 1 || len(bin.execs[0]) != 1 || !strings.HasPrefix(bin.execs[0][0], mergeDoneEnv+"=") {
		t.Fatalf("re-exec attempts %q, want one with %s", bin.execs, mergeDoneEnv)
	}
	var d childDone
	if err := json.Unmarshal([]byte(strings.TrimPrefix(bin.execs[0][0], mergeDoneEnv+"=")), &d); err != nil {
		t.Fatal(err)
	}
	if d.Base != base || d.RC != 4 || strings.TrimSpace(string(d.Doc)) != greenDoc || d.Last != "checks pending" || d.Kept == "" ||
		!d.Owner.Is(owner) || !slices.Equal(d.Command, spec.Command) || d.Gate != spec.Gate {
		t.Errorf("the outcome handed over: %+v", d)
	}
	if !strings.Contains(said, "/opt/beekeeper was replaced while devctl pr wait o/r 7 ran: re-executing it to hand the outcome over") ||
		!strings.Contains(said, "the new binary does not start (text file busy): handing the outcome over under") {
		t.Errorf("stderr:\n%s", said)
	}
	want := "beekeeper gate → s1: devctl pr wait o/r 7 exit 4: green: every check passed (output in " + d.Kept + ")"
	if len(*woke) != 1 || (*woke)[0] != want {
		t.Fatalf("woke %q, want %q", *woke, want)
	}

	// The installed one, re-executed with the outcome: no spec, nothing runs.
	t.Setenv(mergeDoneEnv, strings.TrimPrefix(bin.execs[0][0], mergeDoneEnv+"="))
	if _, err := os.Stat(base + ".spec"); err == nil {
		t.Fatal("the spec is left, the command would run again")
	}
	if err := a.mergeChildRun(context.Background(), base, &fakeBinary{}); err != nil {
		t.Fatalf("unit exit %d (%v), want 0", Code(err), err)
	}
	if len(*woke) != 2 || (*woke)[1] != want {
		t.Errorf("woke %q, want %q twice", *woke, want)
	}
	if _, ok := os.LookupEnv(mergeDoneEnv); ok {
		t.Error("the outcome is left in the environment")
	}
	// Without it, a merge-child with neither spec nor outcome has nobody to tell.
	if err := a.mergeChildRun(context.Background(), base, &fakeBinary{}); len(*woke) != 2 || Code(err) != 127 {
		t.Errorf("woke %q, exit %d", *woke, Code(err))
	}
}

// A waiting gate whose step's save the newer release refuses re-executes
// its replaced binary, keeping its place and deadline, and refuses (77)
// only when the new binary does not start.
func TestAStaleStepReExecutesAReplacedGate(t *testing.T) {
	stubGitHub(t, github.Open, "")
	waiting := state.Merge{Repo: scratchRepo, PR: 3, Lane: scratchRepo, Phase: state.Waiting, By: state.Party{Session: "s2", Name: agentTwo},
		PID: os.Getpid(), Joined: time.Now().UTC(), Seen: time.Now().UTC()}
	a := queueApp(t, waiting)
	newer, err := state.OpenVersion(a.store.Dir(), newerRelease)
	if err != nil {
		t.Fatal(err)
	}
	if err := newer.Update(func(*state.State) ([]state.Event, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if a.store, err = state.OpenVersion(a.store.Dir(), olderRelease); err != nil {
		t.Fatal(err)
	}
	bin := &fakeBinary{replaced: true, execErr: errBusy}
	was := runningBinary
	runningBinary = func() binary { return bin }
	t.Cleanup(func() { runningBinary = was })
	err = a.gate(context.Background(), mergeArgv(scratchRepo), time.Minute, false)
	if Code(err) != ExitGateRefused {
		t.Fatalf("exit %d (%v), want %d", Code(err), err, ExitGateRefused)
	}
	// Before the step and again after its refused save.
	if len(bin.execs) != 2 {
		t.Fatalf("re-exec attempts %q, want two", bin.execs)
	}
	for _, env := range bin.execs {
		if len(env) != 1 || !strings.HasPrefix(env[0], gateDeadlineEnv+"=") {
			t.Errorf("re-exec without the deadline: %q", env)
		}
	}
	if d := lastEventOf(t, a, "merge.refused"); !strings.HasPrefix(d, "o/r#7: this call runs beekeeper v0.1.0, older than the v0.2.0 that wrote the state") {
		t.Errorf("refused event %q", d)
	}
	st, err := newer.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Writer == nil || st.Writer.Version != newerRelease || len(st.Merges) != 1 || st.Merges[0].PR != 3 {
		t.Errorf("writer %+v, merges %+v: the older release's gate touched the state", st.Writer, st.Merges)
	}
}
