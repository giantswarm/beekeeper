package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
)

// A scratch repository and the devctl a release window opened on.
const (
	scratchRepo = "o/r"
	devctlFrom  = "v8.0.0"
)

func TestUntilTime(t *testing.T) {
	now := time.Date(2026, 9, 24, 22, 0, 0, 0, time.Local)
	for in, want := range map[string]time.Time{
		"2h":    now.Add(2 * time.Hour),
		"23:30": time.Date(2026, 9, 24, 23, 30, 0, 0, time.Local),
		"07:00": time.Date(2026, 9, 25, 7, 0, 0, 0, time.Local), // tomorrow morning
	} {
		got, err := untilTime(now, in)
		if err != nil || !got.Equal(want) {
			t.Errorf("untilTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := untilTime(now, "soon"); Code(err) != ExitUsage {
		t.Errorf("untilTime(soon) = %v", err)
	}
}

func TestDiffSnapshots(t *testing.T) {
	reset := time.Now().Add(time.Hour)
	prev := &snapshot{
		Mem:      machine.Mem{AvailableMiB: 40000, SwapUsedMiB: 5000},
		Scope:    &machine.Scope{AnonMiB: 8000, OOMKills: 0},
		Root:     machine.Disk{FreeMiB: 200 * 1024},
		Sessions: []string{"a", "b"},
		Budget:   &github.Budget{Remaining: 4000, Reset: reset},
	}
	cur := &snapshot{
		Mem:      machine.Mem{AvailableMiB: 39000, SwapUsedMiB: 7000},
		Scope:    &machine.Scope{AnonMiB: 8100, OOMKills: 1},
		Root:     machine.Disk{FreeMiB: 180 * 1024},
		Sessions: []string{"b", "c"},
		OOM: []oomKill{{Owner: "x"}, {OOMKill: machine.OOMKill{Memcg: "/user.slice/memcap.slice/memcap-test-1-2.scope"}, Owner: testKillOwner},
			{OOMKill: machine.OOMKill{Memcg: "/user.slice/memcap.slice/memcap-test-1-3.scope"}, Owner: testKillOwner}},
		Budget: &github.Budget{Remaining: 3500, Reset: reset},
	}
	got := strings.Join(diffSnapshots(prev, cur), "\n")
	for _, want := range []string{"swap used 5000 → 7000", "OOM KILL in the desktop scope", "disk / free 200 → 180", "sessions +1: c", "sessions -1: a", "1 kernel OOM kills", "2 test kills in " + testKillOwner, "GitHub budget 4000 → 3500"} {
		if !strings.Contains(got, want) {
			t.Errorf("diff lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "RAM available") {
		t.Errorf("a 1 GiB change of available RAM is noise:\n%s", got)
	}
	if d := diffSnapshots(prev, prev); len(d) != 0 {
		t.Errorf("no change reported as %v", d)
	}
}

func TestGroupKills(t *testing.T) {
	at := time.Date(2026, 9, 24, 19, 57, 0, 0, time.Local)
	var kills []oomKill
	for range 46 {
		kills = append(kills, oomKill{OOMKill: machine.OOMKill{Task: "jest", At: at}, Owner: "memcap"})
	}
	kills = append(kills, oomKill{OOMKill: machine.OOMKill{Task: "node", At: at}, Owner: "kind lab lab-1"})
	got := groupKills(kills)
	if len(got) != 2 || !strings.HasPrefix(got[0], "46 from memcap: jest×46") {
		t.Errorf("groupKills = %v", got)
	}
}

// A cap kill found after its run ended: the scope's process and session are
// gone, the run.start event still names them. The journal lines are the
// machine's own, verbatim.
func TestOOMOwnerNamesAnEndedRun(t *testing.T) {
	raw, err := os.ReadFile("testdata/oom-memcap-2516344.journal")
	if err != nil {
		t.Fatal(err)
	}
	kills := machine.ParseOOM(string(raw))
	if len(kills) != 1 {
		t.Fatalf("kills %+v", kills)
	}
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gone := &proc.Table{ByPID: map[int]*proc.Process{}}
	if got := oomOwner(kills[0], nil, nil, gone, &runIndex{store: store}); got != "memcap scope, cap unknown, owner unknown (no run.start)" {
		t.Errorf("without its run: %q", got)
	}
	by := state.Party{Session: "0f3c", Name: "bk-run-events"}
	if err := store.Log(
		state.Event{By: state.Party{Name: "other"}, Verb: guard.VerbStart, Detail: "memcap-2516344-49287.scope slot 2 max 12G: make lint"},
		state.Event{By: by, Verb: guard.VerbStart, Detail: "memcap-2516344-492870.scope slot 1 max 12G: uv run pytest -n 8"},
	); err != nil {
		t.Fatal(err)
	}
	if got := oomOwner(kills[0], nil, nil, gone, &runIndex{store: store}); got != `memcap cap of "bk-run-events"'s `+"`uv run pytest -n 8`" {
		t.Errorf("with its run: %q", got)
	}
}

// The guard test's own 64M kill, logged to the test's scratch state, before
// its scopes were named memcap-test-: the live log has no run.start for the
// scope, and the kill must not read as the default 12G cap. The journal lines
// are the machine's own, verbatim.
func TestOOMOwnerSaysCapUnknownWithoutARun(t *testing.T) {
	const task = "cmd.test"
	raw, err := os.ReadFile("testdata/oom-memcap-287208.journal")
	if err != nil {
		t.Fatal(err)
	}
	kills := machine.ParseOOM(string(raw))
	if len(kills) != 1 || kills[0].Task != task || kills[0].AnonMiB != 63 {
		t.Fatalf("kills %+v", kills)
	}
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runs := &runIndex{store: store}
	gone := &proc.Table{ByPID: map[int]*proc.Process{}}
	if got := oomOwner(kills[0], nil, nil, gone, runs); got != "memcap scope, cap unknown, owner unknown (no run.start)" {
		t.Errorf("gone: %q", got)
	}
	alive := &proc.Table{ByPID: map[int]*proc.Process{287208: {PID: 287208, Args: []string{task, "run", "--", task, "__alloc"}}}}
	if got := oomOwner(kills[0], nil, nil, alive, runs); got != "memcap scope of `cmd.test run -- cmd.test __alloc`, cap unknown (no run.start)" {
		t.Errorf("alive: %q", got)
	}
	if isTestKill(kills[0]) {
		t.Error("a memcap- scope is not a test's")
	}
}

// A kill in a test run's scope is the test's own, run.start or not.
func TestOOMOwnerNamesATestKill(t *testing.T) {
	raw, err := os.ReadFile("testdata/oom-memcap-287208.journal")
	if err != nil {
		t.Fatal(err)
	}
	k := machine.ParseOOM(strings.ReplaceAll(string(raw), "/memcap-287208-", "/"+guard.TestScopePrefix+"287208-"))[0]
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Log(state.Event{By: state.Party{Name: "capped test"}, Verb: guard.VerbStart, Detail: path.Base(k.Memcg) + " slot 1 max 64M: cmd.test __alloc"}); err != nil {
		t.Fatal(err)
	}
	if !isTestKill(k) {
		t.Fatalf("not a test kill: %+v", k)
	}
	gone := &proc.Table{ByPID: map[int]*proc.Process{}}
	if got := oomOwner(k, nil, nil, gone, &runIndex{store: store}); got != testKillOwner {
		t.Errorf("owner %q", got)
	}
}

func TestIsWait(t *testing.T) {
	const devctl = "devctl"
	yes := [][]string{
		{devctl, "pr", "merge", "giantswarm/x", "1"},
		{devctl, "release", "wait", "giantswarm/x", "--pr", "1"},
		{"gh", "pr", "checks", "1", "--watch"},
		{"docker", "image", "save", "x"},
		{"kubectl", "port-forward", "svc/x", "8080"},
	}
	no := [][]string{{devctl, "version"}, {"gh", "pr", "view", "1"}, {"kubectl", "get", "pods"}}
	for _, args := range yes {
		if !isWait(&proc.Process{Comm: args[0], Args: args}) {
			t.Errorf("%v is a wait", args)
		}
	}
	for _, args := range no {
		if isWait(&proc.Process{Comm: args[0], Args: args}) {
			t.Errorf("%v is no wait", args)
		}
	}
}

func TestMinus(t *testing.T) {
	if got := minus([]string{"a", "b", "c"}, []string{"b"}); !slices.Equal(got, []string{"a", "c"}) {
		t.Errorf("minus = %v", got)
	}
}

func TestUsageExitCode(t *testing.T) {
	t.Setenv("BEEKEEPER_CONFIG", t.TempDir()+"/none.yaml")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, args := range [][]string{{"lease", "claim"}, {"frobnicate"}, {"hold", "frob"}, {"sessions", "extra"}, {"--nope"}} {
		root := New()
		root.SetArgs(args)
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		if err := root.Execute(); Code(err) != ExitUsage {
			t.Errorf("%v: exit %d (%v), want %d", args, Code(err), err, ExitUsage)
		}
	}
}

// self-update reads neither the configuration nor the state, and --check
// answers a newer release with exit 125 and, under --json, the result.
func TestSelfUpdateCheck(t *testing.T) {
	cfg := t.TempDir() + "/broken.yaml"
	if err := os.WriteFile(cfg, []byte("resources: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEEKEEPER_CONFIG", cfg)
	prev := runUpdate
	t.Cleanup(func() { runUpdate = prev })
	var checked bool
	runUpdate = func(_ context.Context, _ io.Writer, check bool) (update.Result, error) {
		checked = check
		return update.Result{Current: "v0.2.0", Latest: "v0.3.0", Newer: true}, update.ErrOutdated
	}

	var out bytes.Buffer
	root := New()
	root.SetArgs([]string{"self-update", "--check", "--json"})
	root.SetOut(&out)
	root.SetErr(io.Discard)
	err := root.Execute()
	if Code(err) != ExitOutdated || !checked || !strings.Contains(err.Error(), "v0.3.0 is newer than v0.2.0") {
		t.Errorf("self-update --check = exit %d, %v (check %v)", Code(err), err, checked)
	}
	var res update.Result
	if jerr := json.Unmarshal(out.Bytes(), &res); jerr != nil || res.Latest != "v0.3.0" || !res.Newer {
		t.Errorf("--json printed %q (%v)", out.String(), jerr)
	}
}

// The hook's third-lab refusal names each holder as `lease list` does: the
// live session's name, not the user@host holder field.
func TestHookNamesLeaseHoldersAsLeaseList(t *testing.T) {
	a := &app{}
	sessions := []*claude.Session{{PID: 7, ID: "s-1", Name: "Agent one"}}
	hs := []lease.Holder{
		{Env: "kind-1", Holder: "teemow@lab", Session: "s-1", Purpose: "e2e", Since: "2026-09-25T10:00:00Z"},
		{Env: "kind-2", Holder: "teemow@lab", Purpose: "by hand", Since: "2026-09-25T11:00:00Z"},
	}
	h := guard.Hook{Self: "/bin/beekeeper", Clusters: func() []string { return []string{"kind-1", "kind-2"} },
		Leases: func() []lease.Holder { return a.namedHolders(sessions, hs) }}
	raw, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "kind create cluster --name third"}, "cwd": "/"})
	var o struct {
		D struct {
			Reason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(h.Decide(raw), &o); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kind-1: Agent one since", "kind-2: teemow@lab since"} {
		if !strings.Contains(o.D.Reason, want) {
			t.Errorf("refusal lacks %q:\n%s", want, o.D.Reason)
		}
	}
	for _, h := range hs {
		if v := a.leaseView(sessions, h); !strings.Contains(o.D.Reason, h.Env+": "+v.Name+" since") {
			t.Errorf("%s: the refusal does not name %q as lease list does", h.Env, v.Name)
		}
	}
}

func TestCheckExcept(t *testing.T) {
	const serving, marge = "lane:serving", "giantswarm/marge"
	a := &app{cfg: &config.Config{Lanes: []config.Lane{{Name: "serving", Repositories: []string{"giantswarm/model-manager"}}}}}
	for _, c := range []struct {
		target, except string
		ok             bool
	}{
		{serving, "giantswarm/model-manager#172", true},
		{serving, "giantswarm/model-manager", true},
		{serving, "giantswarm/marge#3", false},
		{serving, "model-manager#172", false},
		{serving, "giantswarm/model-manager#x", false},
		{marge, marge + "#3", true},
		{marge, marge, false},
		{marge, "giantswarm/backstage#3", false},
		{"merges", "giantswarm/devctl", true},
		{"github", "giantswarm/devctl", false},
		{"github", "", true},
	} {
		if err := a.checkExcept(c.target, c.except); (err == nil) != c.ok {
			t.Errorf("%s except %q: %v", c.target, c.except, err)
		}
	}
}

// stubGitHub answers pullState with state and devctlVersion with version.
func stubGitHub(t *testing.T, pullAt string, version string) *int {
	t.Helper()
	asked := new(int)
	pull, ver, wait := pullState, devctlVersion, judgeWait
	t.Cleanup(func() { pullState, devctlVersion, judgeWait = pull, ver, wait })
	judgeWait = 0
	pullState = func(context.Context, string, int) (github.Pull, error) {
		*asked++
		if pullAt == "" {
			return github.Pull{}, fmt.Errorf("no network")
		}
		return github.Pull{State: pullAt}, nil
	}
	devctlVersion = func(context.Context) string { return version }
	return asked
}
