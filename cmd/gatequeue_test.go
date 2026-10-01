//go:build unix

package cmd

import (
	"context"
	"encoding/json"
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
	"github.com/giantswarm/beekeeper/internal/state"
)

// queueApp is an app with one lane, o/r, whose merges are seeded with
// merges; the caller is the session "worker".
func queueApp(t *testing.T, merges ...state.Merge) *app {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s1")
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_NAME", ownerName)
	store, err := state.Open(t.TempDir())
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
	return &app{cfg: cfg, store: store, now: time.Now()}
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
	noSystemd(t)
	stubGitHub(t, github.Open, "")
	dir := t.TempDir()
	self := filepath.Join(dir, "beekeeper")
	// merge-child's stand-in records its pid and the spec it was given.
	script := "#!/bin/sh\necho $$ > \"$2.pid\"\ncp \"$2.spec\" " + filepath.Join(dir, "spec") + "\n"
	if err := os.WriteFile(self, []byte(script), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	was := selfExe
	selfExe = func() (string, error) { return self, nil }
	t.Cleanup(func() { selfExe = was })
	busy := sleeper(t)
	a := queueApp(t, state.Merge{Repo: scratchRepo, PR: 6, Lane: scratchRepo, By: state.Party{Name: "ahead"}, PID: busy,
		Phase: state.Waiting, Joined: time.Now().Add(-time.Minute), Seen: time.Now()})

	err := a.gate(context.Background(), mergeArgv(scratchRepo), 0, false)
	if Code(err) != ExitGateQueued {
		t.Fatalf("exit %d (%v), want %d", Code(err), err, ExitGateQueued)
	}
	var raw []byte
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if raw, err = os.ReadFile(filepath.Join(dir, "spec")); err == nil && json.Valid(raw) { //nolint:gosec // the test's file
			break
		}
	}
	var spec childSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("no spec: %v", err)
	}
	want := append([]string{self, "gate", "--queued", "--wait", "1h0m0s", "--"}, mergeArgv(scratchRepo)...)
	if spec.Argv[0] != self || !slices.Equal(spec.Argv[1:], want[1:]) {
		t.Errorf("argv %q, want %q", spec.Argv, want)
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

// The queued run takes over the place of the gate it came from; any other
// live gate of the same pull request is refused as a second merge (exit 3)
// and leaves the place alone.
func TestASecondMergeOfAQueuedOneIsRefused(t *testing.T) {
	stubGitHub(t, github.Open, "")
	busy, queued := sleeper(t), sleeper(t)
	seed := func() *app {
		return queueApp(t,
			state.Merge{Repo: scratchRepo, PR: 6, Lane: scratchRepo, By: state.Party{Name: "ahead"}, PID: busy, Phase: state.Waiting, Joined: time.Now().Add(-time.Hour), Seen: time.Now()},
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
