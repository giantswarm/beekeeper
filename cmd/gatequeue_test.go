//go:build unix

package cmd

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// queueApp is an app with one lane, o/r, whose merges are seeded with
// merges; the caller is the session "worker". Its store saves as a dev
// build's, whichever version the test binary is linked with: a stable
// tag's `make test` links the release's version, which would stamp the
// state as its writer and refuse the older releases a test opens.
func queueApp(t *testing.T, merges ...state.Merge) *app {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s1")
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_NAME", ownerName)
	store, err := state.OpenVersion(t.TempDir(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = merges
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Lanes: []config.Lane{{Name: scratchRepo, Repositories: []string{scratchRepo}}}, GitHub: config.GitHub{Floor: 100},
		Merge: config.Merge{Cap: 4, QueueTTL: config.Duration{Duration: 15 * time.Minute}, SeedTTL: config.Duration{Duration: time.Hour},
			DevctlOwners: []string{"o"}}}
	hostDevctls(t, 0)
	return &app{cfg: cfg, store: store, now: time.Now()}
}

// devctlMachine is a machine whose process table holds n devctl processes
// and nothing else.
type devctlMachine struct {
	platform.Machine
	n int
}

func (m devctlMachine) Processes() (*proc.Table, error) {
	t := &proc.Table{ByPID: map[int]*proc.Process{}}
	for pid := 1; pid <= m.n; pid++ {
		t.ByPID[pid] = &proc.Process{PID: pid, PPID: 1, Comm: merge.Tool}
	}
	return t, nil
}

// hostDevctls makes the gate count n devctl processes machine-wide instead
// of the host's own.
func hostDevctls(t *testing.T, n int) {
	t.Helper()
	was := plat.Machine
	plat.Machine = devctlMachine{Machine: was, n: n}
	t.Cleanup(func() { plat.Machine = was })
}

// stubSelf stands in for this binary as merge-child: it records its pid and
// the spec it was given, which launched returns.
func stubSelf(t *testing.T) (self string, launched func() childSpec) {
	t.Helper()
	noSystemd(t)
	dir := t.TempDir()
	self = filepath.Join(dir, "beekeeper")
	script := "#!/bin/sh\necho $$ > \"$2.pid\"\ncp \"$2.spec\" " + filepath.Join(dir, "spec") + "\n"
	if err := os.WriteFile(self, []byte(script), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	was := selfExe
	selfExe = func() (string, error) { return self, nil }
	t.Cleanup(func() { selfExe = was })
	return self, func() childSpec {
		t.Helper()
		var raw []byte
		var err error
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if raw, err = os.ReadFile(filepath.Join(dir, "spec")); err == nil && json.Valid(raw) { //nolint:gosec // the test's file
				break
			}
		}
		var spec childSpec
		if err := json.Unmarshal(raw, &spec); err != nil {
			t.Fatalf("no spec: %v", err)
		}
		return spec
	}
}

// aheadName is the session of the merge ahead in a busy lane.
const aheadName = "ahead"

// busyLaneApp is queueApp with o/r#6 of a live gate waiting ahead.
func busyLaneApp(t *testing.T) *app {
	t.Helper()
	return queueApp(t, state.Merge{Repo: scratchRepo, PR: 6, Lane: scratchRepo, By: state.Party{Name: aheadName}, PID: sleeper(t),
		Phase: state.Waiting, Joined: time.Now().Add(-time.Minute), Seen: time.Now()})
}

// sleeper is a live process that is not the test, ended with the test.
func sleeper(t *testing.T) int {
	t.Helper()
	c := exec.Command("sleep", "60")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	return c.Process.Pid
}

// A merge behind a busy lane does not end in a rerun: at its deadline the
// gate hands its wait to a run of its own, the same gate under --queued with
// the caller as owner, and exits 76.
func TestAMergeBehindABusyLaneWaitsOnInARunOfItsOwn(t *testing.T) {
	stubGitHub(t, github.Open, "")
	self, launched := stubSelf(t)
	a := busyLaneApp(t)

	err := a.gate(context.Background(), mergeArgv(scratchRepo), 0, false)
	if Code(err) != ExitGateQueued {
		t.Fatalf("exit %d (%v), want %d", Code(err), err, ExitGateQueued)
	}
	spec := launched()
	want := append([]string{self, "gate", "--queued", "--wait", "1h0m0s", "--"}, mergeArgv(scratchRepo)...)
	if !slices.Equal(spec.Argv, want) || !slices.Equal(spec.Command, mergeArgv(scratchRepo)) {
		t.Errorf("argv %q, command %q, want %q", spec.Argv, spec.Command, want)
	}
	if spec.Owner.Session != "s1" || spec.Gate != os.Getpid() {
		t.Errorf("owner %+v, gate %d", spec.Owner, spec.Gate)
	}
	if !slices.Contains(spec.Env, gateFromEnv+"="+strconv.Itoa(os.Getpid())) {
		t.Errorf("the queued run does not know the gate it takes the place from")
	}
	evs, _ := a.store.Events(0, func(e state.Event) bool { return e.Verb == "merge.queued" })
	if len(evs) != 2 || !strings.Contains(evs[1].Detail, "waits on in lane o/r in a run of its own") {
		t.Errorf("events %+v", evs)
	}
	st, _ := a.store.Read()
	if i := slices.IndexFunc(st.Merges, func(m state.Merge) bool { return m.PR == 7 && m.Phase == state.Waiting }); i < 0 {
		t.Errorf("the merge lost its place: %+v", st.Merges)
	}
}

// A detached merge is the blocking one under the gate: --detach and its
// --on-done never reach the run that merges, which the lane accounts for.
func TestAGatedDetachedMergeRunsBlocking(t *testing.T) {
	stubGitHub(t, github.Open, "")
	self, launched := stubSelf(t)
	a := busyLaneApp(t)

	argv := mergeArgv(scratchRepo, "--detach", "--on-done", "beekeeper agents wake x")
	if err := a.gate(context.Background(), argv, 0, false); Code(err) != ExitGateQueued {
		t.Fatalf("exit %d (%v), want %d", Code(err), err, ExitGateQueued)
	}
	spec := launched()
	want := append([]string{self, "gate", "--queued", "--wait", "1h0m0s", "--"}, mergeArgv(scratchRepo)...)
	if !slices.Equal(spec.Argv, want) || !slices.Equal(spec.Command, mergeArgv(scratchRepo)) {
		t.Errorf("argv %q, command %q, want %q", spec.Argv, spec.Command, want)
	}
}

// The queued run takes over the place of the gate it came from; any other
// live gate of the same pull request is refused as a second merge (exit 3)
// and leaves the place alone.
func TestASecondMergeOfAQueuedOneIsRefused(t *testing.T) {
	stubGitHub(t, github.Open, "")
	busy, queued := sleeper(t), sleeper(t)
	seed := func() *app {
		return queueApp(t,
			state.Merge{Repo: scratchRepo, PR: 6, Lane: scratchRepo, By: state.Party{Name: aheadName}, PID: busy, Phase: state.Waiting, Joined: time.Now().Add(-time.Hour), Seen: time.Now()},
			state.Merge{Repo: scratchRepo, PR: 7, Lane: scratchRepo, By: state.Party{Session: "s1", Name: ownerName}, PID: queued, Phase: state.Waiting, Joined: time.Now(), Seen: time.Now()})
	}
	a := seed()
	if err := a.gate(context.Background(), mergeArgv(scratchRepo), 0, false); Code(err) != ExitGateDuplicate {
		t.Fatalf("exit %d (%v), want %d", Code(err), err, ExitGateDuplicate)
	}
	st, _ := a.store.Read()
	if i := slices.IndexFunc(st.Merges, func(m state.Merge) bool { return m.PR == 7 && m.PID == queued }); i < 0 {
		t.Errorf("the queued run lost its place: %+v", st.Merges)
	}

	a = seed()
	t.Setenv(gateFromEnv, strconv.Itoa(queued))
	err := a.gate(context.Background(), mergeArgv(scratchRepo), 0, true)
	if Code(err) != ExitGateRefused {
		t.Fatalf("the queued run past its wait: exit %d (%v), want %d", Code(err), err, ExitGateRefused)
	}
	if d := lastEventOf(t, a, "merge.refused"); !strings.Contains(d, "waited 0s for its turn, position 2 in lane o/r behind o/r#6") {
		t.Errorf("refusal %q", d)
	}
}

func lastEventOf(t *testing.T, a *app, verb string) string {
	t.Helper()
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == verb })
	if err != nil || len(evs) == 0 {
		return ""
	}
	return evs[len(evs)-1].Detail
}

// A place whose merge is not in the gate leaves its lane once its pull
// request closes and settles it once merged; a place whose gate waits, and
// one asked less than a check ago, are not asked.
func TestClosedAndMergedPlacesLeaveTheirLane(t *testing.T) {
	now := time.Now()
	alive := sleeper(t)
	a := queueApp(t,
		state.Merge{Repo: scratchRepo, PR: 1, Lane: scratchRepo, Phase: state.Waiting, Seeded: true, Seen: now},
		state.Merge{Repo: scratchRepo, PR: 2, Lane: scratchRepo, Phase: state.Waiting, Seen: now, Finished: now, Exit: 1},
		state.Merge{Repo: scratchRepo, PR: 3, Lane: scratchRepo, Phase: state.Waiting, Seen: now},
		state.Merge{Repo: scratchRepo, PR: 4, Lane: scratchRepo, Phase: state.Waiting, PID: alive, Seen: now},
		state.Merge{Repo: scratchRepo, PR: 5, Lane: scratchRepo, Phase: state.Waiting, Seen: now, Checked: now.Add(-time.Second)})
	pulls := map[int]github.Pull{1: {State: github.Closed}, 2: {State: github.Merged, MergedAt: now}, 3: {State: github.Open}}
	was := pullState
	t.Cleanup(func() { pullState = was })
	var asked []int
	pullState = func(_ context.Context, _ string, n int) (github.Pull, error) {
		asked = append(asked, n)
		return pulls[n], nil
	}
	if err := a.checkPlaces(context.Background(), watchParty, now, "", "", 0, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(asked, []int{1, 2, 3}) {
		t.Errorf("asked about %v, want [1 2 3]", asked)
	}
	st, _ := a.store.Read()
	phase := map[int]string{}
	for _, m := range st.Merges {
		phase[m.PR] = m.Phase
	}
	if _, ok := phase[1]; ok || phase[2] != state.Settling || phase[3] != state.Waiting || phase[4] != state.Waiting || phase[5] != state.Waiting {
		t.Errorf("phases %v", phase)
	}
	if d := lastEventOf(t, a, "merge.dropped"); d != "o/r#1: closed without a merge, its place in lane o/r is dropped" {
		t.Errorf("dropped %q", d)
	}
	if d := lastEventOf(t, a, "merged"); !strings.HasPrefix(d, "o/r#2 outside the gate at ") {
		t.Errorf("merged %q", d)
	}
}

// A merge under the budget floor is not refused: it is queued for the reset
// in a run of its own (exit 76, a "queued" line), which waits while the
// budget is under the floor.
func TestAMergeUnderTheBudgetFloorIsQueuedForTheReset(t *testing.T) {
	stubGitHub(t, github.Open, "")
	_, launched := stubSelf(t)
	a := queueApp(t)
	a.cfg.Merge.BudgetFresh = config.Duration{Duration: time.Hour}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Budget = &state.Budget{Remaining: 40, Limit: 5000, Reset: time.Now().Add(30 * time.Minute), At: time.Now()}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	var err error
	stderr := gateStderr(t, func() { err = a.gate(context.Background(), mergeArgv(scratchRepo), time.Minute, false) })
	if Code(err) != ExitGateQueued {
		t.Fatalf("exit %d (%v), want %d", Code(err), err, ExitGateQueued)
	}
	if !strings.Contains(stderr, GatePrefix+"queued, the GitHub budget 40 is under the floor 100") ||
		!strings.Contains(stderr, "its outcome wakes") || strings.Contains(stderr, "refused") {
		t.Errorf("stderr %q, want the queued line", stderr)
	}
	if spec := launched(); !slices.Contains(spec.Argv, "--queued") {
		t.Errorf("not queued: %q", spec.Argv)
	}
	if d := lastEventOf(t, a, "merge.queued"); !strings.Contains(d, "the GitHub budget 40 is under the floor 100 until the reset") {
		t.Errorf("queued event %q", d)
	}
	// The queued run waits under the floor instead of refusing.
	g := &gateRun{app: a, ctx: context.Background(), argv: mergeArgv(scratchRepo), repo: scratchRepo, pr: 7, lane: a.cfg.LaneOf(scratchRepo),
		me: state.Party{Session: "s1", Name: ownerName}, pid: os.Getpid(), queued: true}
	if why, err := g.step(); err != nil || !strings.Contains(why, "under the floor") {
		t.Errorf("queued run: %q, %v; want a wait for the reset", why, err)
	}
}

// A held repository is refused (exit 77) with its reason, and nothing is
// queued: 77 never comes with a "queued" line.
func TestAHeldMergeIsRefusedNotQueued(t *testing.T) {
	stubGitHub(t, github.Open, "")
	a := queueApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Holds = []state.Hold{{Target: scratchRepo, By: state.Party{Name: aheadName}, At: time.Now(), Reason: "a test hold"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	var err error
	stderr := gateStderr(t, func() { err = a.gate(context.Background(), mergeArgv(scratchRepo), time.Minute, false) })
	if Code(err) != ExitGateRefused {
		t.Fatalf("exit %d (%v), want %d", Code(err), err, ExitGateRefused)
	}
	if !strings.Contains(stderr, GatePrefix+"refused, o/r is held") || !strings.Contains(stderr, "a test hold") || strings.Contains(stderr, "queued") {
		t.Errorf("stderr %q, want the refusal with its reason", stderr)
	}
	if d := lastEventOf(t, a, "merge.queued"); strings.Contains(d, "run of its own") {
		t.Errorf("a refused merge was queued: %q", d)
	}
}

// gateStderr runs f and returns what it wrote to stderr.
func gateStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	was := os.Stderr
	os.Stderr = w
	f()
	os.Stderr = was
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}
