package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/free"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	tmpfsDead = "11111111-dead-4000-8000-000000000000"
	tmpfsLive = "22222222-a11e-4000-8000-000000000000"
)

// tmpfsApp is an app freeing tmpfs every hour, and a machine whose /tmp is a
// test dir holding a dead session's 2 MiB scratch, a live session's and a
// 3 MiB go-build dir no go command owns, all untouched for five hours.
func tmpfsApp(t *testing.T) (*app, func() (free.Machine, error), string) {
	t.Helper()
	tmp := t.TempDir()
	now := time.Now()
	for rel, kib := range map[string]int{
		"claude-1000/-p/" + tmpfsDead + "/scratchpad/a": 2048,
		"claude-1000/-p/" + tmpfsLive + "/scratchpad/a": 2048,
		"go-build1/b001/a": 3072,
	} {
		p := filepath.Join(tmp, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, kib<<10), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-5*time.Hour), now.Add(-5*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{StateDir: dir, Doctor: config.Doctor{FreeEvery: config.Duration{Duration: time.Hour}}}
	machine := func() (free.Machine, error) {
		return free.Machine{
			UID:      os.Getuid(),
			TmpDir:   tmp,
			Scratch:  filepath.Join(tmp, "claude-1000"),
			Projects: filepath.Join(tmp, "projects"),
			Table:    &proc.Table{ByPID: map[int]*proc.Process{}},
			Sessions: []*claude.Session{{PID: 4242, ID: tmpfsLive, Name: "Agent one"}},
			Desk:     &state.State{},
			Now:      now,
		}, nil
	}
	return &app{cfg: cfg, store: store, now: now, out: &bytes.Buffer{}}, machine, tmp
}

func TestFreeTmpfsFreesTheDeadAndLogs(t *testing.T) {
	a, machine, tmp := tmpfsApp(t)
	r := a.freeTmpfs(false, machine)
	const want = "tmpfs: freed 1 dead session's dir, 2 MiB; 1 orphaned go-build dir, 3 MiB"
	if r.String() != want || !r.notable() {
		t.Errorf("run = %q, want %q", r, want)
	}
	for rel, kept := range map[string]bool{"claude-1000/-p/" + tmpfsDead: false, "claude-1000/-p/" + tmpfsLive: true, "go-build1": false} {
		if exists(filepath.Join(tmp, rel)) != kept {
			t.Errorf("%s kept = %v, want %v", rel, !kept, kept)
		}
	}
	evs, err := a.store.Events(10, func(e state.Event) bool { return e.Verb == freeVerb })
	if err != nil || len(evs) != 1 || evs[0].Detail != r.what() {
		t.Errorf("logged %+v, %v; want one %s event saying what it freed", evs, err, freeVerb)
	}
}

func TestFreeTmpfsDryRunRemovesNothing(t *testing.T) {
	a, machine, tmp := tmpfsApp(t)
	r := a.freeTmpfs(true, machine)
	if !strings.HasPrefix(r.String(), "tmpfs: would free 1 dead session's dir, 2 MiB") {
		t.Errorf("run = %q", r)
	}
	if !exists(filepath.Join(tmp, "claude-1000/-p/"+tmpfsDead)) || !exists(filepath.Join(tmp, "go-build1")) {
		t.Error("a dry run removed a dir")
	}
	if evs, _ := a.store.Events(10, func(e state.Event) bool { return e.Verb == freeVerb }); len(evs) != 0 {
		t.Errorf("a dry run logged %+v", evs)
	}
}

func TestFreeTmpfsLeavesItToTheRunUnderWay(t *testing.T) {
	a, machine, tmp := tmpfsApp(t)
	lock := flock.New(filepath.Join(a.store.Dir(), freeLock))
	if ok, err := lock.TryLock(); !ok || err != nil {
		t.Fatal(ok, err)
	}
	defer func() { _ = lock.Unlock() }()
	if r := a.freeTmpfs(false, machine); !r.locked || !exists(filepath.Join(tmp, "go-build1")) {
		t.Fatalf("run = %+v; want it left to the lock's holder", r)
	}
}

// The watch frees once per doctor.freeEvery, says FREED when it freed
// something, keeps the time for every watch and says it on the TMPFS line.
func TestWatchFreesTmpfsOncePerPeriod(t *testing.T) {
	a, machine, _ := tmpfsApp(t)
	w := a.newWatcher(false, false)
	if got := w.lastFreeNote(); got != "the automatic free has not run yet" {
		t.Errorf("before a run: %q", got)
	}
	now := time.Date(2026, 10, 13, 6, 0, 0, 0, time.Local)
	w.freeDue(machine, now, time.Hour)
	out := a.out.(*bytes.Buffer).String()
	if !strings.Contains(out, "FREED in tmpfs: 1 dead session's dir, 2 MiB; 1 orphaned go-build dir, 3 MiB (doctor.freeEvery)") {
		t.Errorf("watch said:\n%s", out)
	}
	if got := w.lastFreeNote(); got != "the automatic free ran at 06:00 and freed 1 dead session's dir, 2 MiB; 1 orphaned go-build dir, 3 MiB" {
		t.Errorf("after a run: %q", got)
	}
	// Within the hour another watch reads the run and waits for its hour.
	other := a.newWatcher(true, false)
	other.freeDue(machine, now.Add(30*time.Minute), time.Hour)
	if !other.freeNext.Equal(now.Add(time.Hour)) || strings.Count(a.out.(*bytes.Buffer).String(), "FREED") != 1 {
		t.Errorf("a second run within the hour: next %v\n%s", other.freeNext, a.out)
	}
	// An hour on there is nothing left to free: no line, the note says so.
	w.freeDue(machine, now.Add(time.Hour), time.Hour)
	if got := w.lastFreeNote(); got != "the automatic free ran at 07:00, nothing to free" || strings.Count(a.out.(*bytes.Buffer).String(), "FREED") != 1 {
		t.Errorf("an empty run: %q\n%s", got, a.out)
	}
	a.cfg.Doctor.FreeEvery.Duration = -1
	if got := w.lastFreeNote(); got != "the automatic free is off (doctor.freeEvery)" {
		t.Errorf("off: %q", got)
	}
}
