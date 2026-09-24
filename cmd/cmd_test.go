package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/update"
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
		OOM:      []oomKill{{Owner: "x"}},
		Budget:   &github.Budget{Remaining: 3500, Reset: reset},
	}
	got := strings.Join(diffSnapshots(prev, cur), "\n")
	for _, want := range []string{"swap used 5000 → 7000", "OOM KILL in the desktop scope", "disk / free 200 → 180", "sessions +1: c", "sessions -1: a", "1 kernel OOM kills", "GitHub budget 4000 → 3500"} {
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
