package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
)

// localCap is a capacity whose capacity.local.ceiling is n.
func localCap(ceiling, n int) config.Capacity {
	return config.Capacity{Floor: 0, Ceiling: ceiling, Local: config.LocalCapacity{Ceiling: &n}}
}

// remoteTasked is a busy agent whose session runs on an execution host.
func remoteTasked(name string) state.Agent {
	ag := tasked(name)
	ag.Execution = &execution.Ref{Host: execution.HostID(sshTestSelector), Execution: execution.ExecutionID("x-" + strings.ReplaceAll(name, " ", "-")), Generation: 1}
	return ag
}

// capacity.local.ceiling bounds the busy agents this machine runs: an agent
// on an execution host weighs on capacity.ceiling (the subscriptions) but
// never on the local cap, and the cap narrows the room capacity.ceiling
// leaves. Unset, nothing changes.
func TestLocalCeilingCountsOnlyThisMachine(t *testing.T) {
	st := supervised(3)
	st.Agents = append(st.Agents, remoteTasked("remote 1"), remoteTasked("remote 2"))
	c := countAgents(st, nil, keepNow, nil)
	if len(c.Busy) != 5 {
		t.Fatalf("busy %v, want the three local and two remote agents", c.Busy)
	}

	unset := config.Capacity{Ceiling: 14}
	if v := newCapacity(c, unset, roomy(), false).withLocal(st, unset); v.Local != nil || v.Room != 9 {
		t.Errorf("unset local ceiling: local %+v, room %d, want none and 9", v.Local, v.Room)
	}

	k := localCap(14, 5)
	v := newCapacity(c, k, roomy(), false).withLocal(st, k)
	if v.Local == nil || len(v.Local.Busy) != 3 || v.Local.Room != 2 || v.Room != 2 {
		t.Fatalf("local ceiling 5: local %+v, room %d, want 3 busy and room 2", v.Local, v.Room)
	}
	if slices.Contains(v.Local.Busy, "remote 1") {
		t.Errorf("a remote agent counts locally: %v", v.Local.Busy)
	}
	if got := v.Local.line(); got != "3 busy of ceiling 5: BK 0, BK 1, BK 2" {
		t.Errorf("local line %q", got)
	}

	full := localCap(14, 3)
	v = newCapacity(c, full, roomy(), false).withLocal(st, full)
	if want := "no start: 3 busy locally at capacity.local.ceiling 3"; v.Room != 0 || v.verdict() != want {
		t.Errorf("at the local ceiling: room %d, %q, want %q", v.Room, v.verdict(), want)
	}
	if _, err := gateStart(v, false); Code(err) != ExitRefused {
		t.Errorf("gateStart at the local ceiling = %v, want a refusal", err)
	}

	zero := localCap(14, 0)
	if v = newCapacity(c, zero, roomy(), false).withLocal(st, zero); v.Room != 0 || v.verdict() != "no start: capacity.local.ceiling 0" {
		t.Errorf("local ceiling 0: room %d, %q", v.Room, v.verdict())
	}

	// The global ceiling still binds below the local one.
	tight := localCap(6, 5)
	if v = newCapacity(c, tight, roomy(), false).withLocal(st, tight); v.Room != 1 {
		t.Errorf("capacity.ceiling 6 under local room 2: room %d, want 1", v.Room)
	}
}

// forTarget narrows a verdict to an explicit harness's provider: the local
// cap still holds after it, even where the provider's own room replaces
// the pool's.
func TestLocalCeilingHoldsForAnExplicitHarness(t *testing.T) {
	k := config.Capacity{Ceiling: 10, Providers: map[string]config.Provider{claudeComm: {Ceiling: 5}}, Choices: []config.Choice{{Harness: claudeComm, Model: bgModel}}}
	for _, tc := range []struct {
		local, want int
	}{{0, 0}, {1, 1}, {4, 3}} {
		v := &capacityView{Providers: map[string]*providerView{claudeComm: {Name: claudeComm, Room: 3}}, Headroom: roomy(), Local: &localView{Busy: []string{"BK 0"}, Ceiling: tc.local + 1, Room: tc.local}}
		v = v.forTarget(k, claudeComm, bgModel)
		if v.Room != tc.want {
			t.Errorf("local room %d: room %d, want %d (%v)", tc.local, v.Room, tc.want, v.Blocks)
		}
		if tc.want == 0 && !strings.Contains(v.verdict(), "capacity.local.ceiling") {
			t.Errorf("local room 0: verdict %q names no local ceiling", v.verdict())
		}
	}
}

// The hand start at the local cap: capacity prints the cap beside the other
// limits, agents start refuses a worker and a reviewer alike, a remote busy
// agent leaves the local room alone, and --force still goes through the gate.
func TestLocalCeilingRefusesAHandStart(t *testing.T) {
	orig := plat
	plat = platform.Stub()
	plat.Machine = roomyMachine{Machine: plat.Machine}
	t.Cleanup(func() { plat = orig })

	dir := t.TempDir()
	cfgPath := emptyTestConfig(t, filepath.Join(dir, "config.yaml"))
	t.Setenv("HOME", dir)
	t.Setenv("BEEKEEPER_CONFIG", cfgPath)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("MEMCAP_STATE", filepath.Join(dir, "memcap"))
	t.Setenv("PATH", dir)
	if _, err := config.Set(cfgPath, []config.Setting{{Key: "capacity.ceiling", Value: "14"}, {Key: "capacity.local.ceiling", Value: "1"}}); err != nil {
		t.Fatal(err)
	}
	openApp := func() (*app, *bytes.Buffer) {
		c, err := config.Load(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		store, err := state.OpenVersion(c.StateDir, "dev")
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		return &app{cfgPath: cfgPath, cfg: c, store: store, now: keepNow, out: &out, as: agentOne}, &out
	}
	a, out := openApp()
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, remoteTasked("remote 1"))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.capacityCmd().Execute(); err != nil || !strings.Contains(out.String(), "local: 0 busy of ceiling 1\n") || !strings.Contains(out.String(), "room for 1 start\n") {
		t.Fatalf("a remote agent took the local room: %q, %v", out.String(), err)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, tasked("BK 0"))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	a, out = openApp()
	const block = "no start: 1 busy locally at capacity.local.ceiling 1"
	if err := a.capacityCmd().Execute(); err != nil || !strings.Contains(out.String(), "local: 1 busy of ceiling 1: BK 0\n") || !strings.Contains(out.String(), block+"\n") {
		t.Fatalf("capacity at the local ceiling = %q, %v", out.String(), err)
	}
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("Work the task.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{nil, {"--" + reviewFlag}} {
		start := a.agentStartCmd()
		start.SilenceErrors, start.SilenceUsage = true, true
		start.SetArgs(append([]string{busyOne, brief, "--dir", dir}, extra...))
		if err := start.Execute(); Code(err) != ExitRefused || !strings.Contains(err.Error(), block) {
			t.Errorf("agents start %v at the local ceiling = exit %d (%v), want a refusal naming it", extra, Code(err), err)
		}
	}
	stubBackground(t, "backgrounded · "+bgShortID+" · "+busyOne+"\n", bgSession{ID: bgShortID, SessionID: bgID, Name: busyOne})
	force := a.agentStartCmd()
	force.SilenceErrors, force.SilenceUsage = true, true
	force.SetArgs([]string{busyOne, brief, "--dir", dir, modeFlag, config.ModeBackground, "--force"})
	if err := force.Execute(); err != nil {
		t.Fatalf("agents start --force at the local ceiling = %v", err)
	}
}
