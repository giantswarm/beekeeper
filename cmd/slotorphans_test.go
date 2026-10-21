package cmd

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// slotMachine is a machine whose build slot slices run the scopes, each
// with its processes; a scope's cgroup path is its name.
type slotMachine struct {
	platform.Machine
	slices map[string][]string
	pids   map[string][]int
}

func (m *slotMachine) SlotSlice(slice string) *machine.SlotSlice {
	if s, ok := m.slices[slice]; ok {
		return &machine.SlotSlice{Scopes: s}
	}
	return nil
}

func (m *slotMachine) MemcapScope(unit string) *machine.Scope {
	if _, ok := m.pids[unit]; ok {
		return &machine.Scope{Path: unit}
	}
	return nil
}

func (m *slotMachine) ScopePIDs(path string) []int { return m.pids[path] }

// scopeStopper stops a scope of the slot machine as the service manager
// does: the scope and its processes are gone.
type scopeStopper struct {
	platform.Launcher
	m       *slotMachine
	stopped []string
}

func (l *scopeStopper) Stop(_ context.Context, name string) error {
	l.stopped = append(l.stopped, name)
	delete(l.m.pids, name)
	for slice, scopes := range l.m.slices {
		l.m.slices[slice] = slices.DeleteFunc(scopes, func(s string) bool { return s == name })
	}
	return nil
}

const (
	liveSlotScope   = "memcap-100-000001.scope"
	orphanSlotScope = "memcap-200-000002.scope"
)

// A slot scope whose run is gone (an envtest kube-apiserver the service
// manager adopted) is named at the first sample, stopped once it has run on
// for the grace, and the slot is free; a live run's scope in the same slice
// is left alone. A --once watch names it and stops nothing.
func TestWatchStopsAnOrphanedSlotScope(t *testing.T) {
	t.Setenv("MEMCAP_STATE", "")
	t.Setenv("MEMCAP_SLOTS", "")
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	slotDir := t.TempDir()
	cfg := &config.Config{StateDir: dir, Memcap: config.Memcap{SlotDir: slotDir, Slots: 1},
		Watch: config.Watch{Repeat: config.Duration{Duration: time.Hour}}}
	out := &bytes.Buffer{}
	a := &app{cfg: cfg, store: store, now: time.Now(), out: out}
	m := &slotMachine{Machine: plat.Machine,
		slices: map[string][]string{guard.Options{SlotDir: slotDir}.SlotSlice(1): {liveSlotScope, orphanSlotScope}},
		pids:   map[string][]int{liveSlotScope: {101, 102}, orphanSlotScope: {300}},
	}
	l := &scopeStopper{m: m}
	savedM, savedL := plat.Machine, plat.Launcher
	plat.Machine, plat.Launcher = m, l
	t.Cleanup(func() { plat.Machine, plat.Launcher = savedM, savedL })
	table := proc.NewTable(
		&proc.Process{PID: 900, PPID: 1, Comm: "systemd"},
		&proc.Process{PID: 100, PPID: 50, Comm: "beekeeper"},
		&proc.Process{PID: 101, PPID: 100, Comm: "zsh"},
		&proc.Process{PID: 102, PPID: 101, Comm: "go"},
		&proc.Process{PID: 300, PPID: 900, Comm: "kube-apiserver"},
	)
	t0 := time.Date(2026, 10, 21, 10, 0, 0, 0, time.Local)

	once := a.newWatcher(false, false)
	once.slotOrphans(t0, table)
	once.slotOrphans(t0.Add(2*slotOrphanGrace), table)
	named := "ORPHANED SLOT SCOPE: slot 1's " + orphanSlotScope + " runs on without its run: 300 (kube-apiserver); it keeps slot 1 busy, the watch stops it at 10:01:00"
	if got := out.String(); strings.Count(got, named) != 1 || strings.Contains(got, liveSlotScope) || len(l.stopped) != 0 {
		t.Fatalf("a --once watch said:\n%s\nstopped %v; want the orphan named once, nothing stopped", got, l.stopped)
	}

	out.Reset()
	w := a.newWatcher(false, false)
	w.slotsOn = true
	w.slotOrphans(t0, table)
	w.slotOrphans(t0.Add(slotOrphanGrace/2), table)
	if got := out.String(); strings.Count(got, named) != 1 || len(l.stopped) != 0 {
		t.Fatalf("within the grace the watch said:\n%s\nstopped %v; want the orphan named once, nothing stopped", got, l.stopped)
	}
	w.slotOrphans(t0.Add(slotOrphanGrace), table)
	if !slices.Equal(l.stopped, []string{orphanSlotScope}) {
		t.Fatalf("stopped %v, want only %s", l.stopped, orphanSlotScope)
	}
	stopped := "STOPPED orphaned slot scope " + orphanSlotScope + " of slot 1: 300 (kube-apiserver); slot 1 is free"
	if got := out.String(); !strings.Contains(got, stopped) || !strings.Contains(got, "ENDED ORPHANED SLOT SCOPE") {
		t.Errorf("after the grace the watch said:\n%s\nwant %q and the ENDED line", got, stopped)
	}
	out.Reset()
	w.slotOrphans(t0.Add(2*slotOrphanGrace), table)
	if out.Len() != 0 || len(l.stopped) != 1 {
		t.Errorf("after the stop the watch said %q, stopped %v; want nothing more", out.String(), l.stopped)
	}
}

func TestOrphanedScope(t *testing.T) {
	table := proc.NewTable(
		&proc.Process{PID: 900, PPID: 1, Comm: "systemd"},
		&proc.Process{PID: 100, PPID: 50, Comm: "beekeeper"},
		&proc.Process{PID: 101, PPID: 100, Comm: "zsh"},
		&proc.Process{PID: 102, PPID: 101, Comm: "go"},
		&proc.Process{PID: 300, PPID: 900, Comm: "kube-apiserver"},
		&proc.Process{PID: 301, PPID: 300, Comm: "etcd"},
		&proc.Process{PID: 400, PPID: 1, Comm: "sleep"},
	)
	for _, c := range []struct {
		name string
		pids []int
		want bool
	}{
		{"a live run's command and its child", []int{101, 102}, false},
		{"an adopted process and its child", []int{300, 301}, true},
		{"adopted by PID 1", []int{400}, true},
		{"a live run's command beside an adopted one", []int{101, 300}, false},
		{"processes gone from the table", []int{7, 8}, false},
		{"none", nil, false},
	} {
		if got := orphanedScope(table, c.pids); got != c.want {
			t.Errorf("%s: orphanedScope(%v) = %v, want %v", c.name, c.pids, got, c.want)
		}
	}
}
