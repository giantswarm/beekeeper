package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// unusedFor starts the verdict of an eligible holder.
const unusedFor = "unused for"

// holdersFixture is a state home with one holder of each kind, all last
// written eight days ago unless a test touches them.
func holdersFixture(t *testing.T) (*app, string, holderMachine) {
	t.Helper()
	a, _ := stubApp(t)
	a.now = time.Now()
	root := t.TempDir()
	a.cfg.Disk.Holders.Root = root
	a.cfg.Disk.Holders.Keep = []string{"hf-cache"}
	a.cfg.StateDir = filepath.Join(root, pluginName)
	old := a.now.Add(-8 * 24 * time.Hour)
	files := []string{
		pluginName + "/state.json",
		"cargo-target-upstream/debug/big",
		"ao1011/notes.md",
		"ku807/scratch",
		"go-build-153/ab/cd",
		"hf-cache/model",
		"inuse-holder/data",
		"checkout-holder/worktrees/x/.git",
		"aside/backstage-3125-second/.git",
		"aside/fresh/file",
		"moved-aside/kd-3131/file",
		"loose.log",
	}
	for _, p := range files {
		writeT(t, filepath.Join(root, p), strings.Repeat("x", 8192))
	}
	for _, p := range files {
		for ; p != "."; p = filepath.Dir(p) {
			_ = os.Chtimes(filepath.Join(root, p), old, old)
		}
	}
	recent := a.now.Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(root, "aside/fresh/file"), recent, recent)
	m := holderMachine{
		refs: []string{filepath.Join(root, "inuse-holder", "data")},
		st: &state.State{
			Agents: []state.Agent{
				{Party: state.Party{Name: "BK 807 open"}, Task: "Work https://github.com/giantswarm/klaus/issues/807"},
				{Party: state.Party{Name: "BK 153 done"}, LastTask: "Work https://github.com/giantswarm/beekeeper/issues/153", Done: true},
			},
			Records: []state.Record{
				{Session: state.Party{Name: "AO 1011"}, Issue: "giantswarm/agent-platform#1011", Ended: old},
			},
		},
	}
	return a, root, m
}

func TestListHoldersOwnersAndEligibility(t *testing.T) {
	a, root, m := holdersFixture(t)
	r := a.listHolders(func() (holderMachine, error) { return m, nil })
	if r.err != nil {
		t.Fatal(r.err)
	}
	got := map[string]holder{}
	for _, h := range r.Holders {
		got[h.Name] = h
	}
	for _, tc := range []struct {
		name, owner, why string
		eligible         bool
	}{
		{pluginName, pluginName, "beekeeper's own state", false},
		{"cargo-target-upstream", "program cargo-target-upstream", unusedFor, true},
		{"ao1011", "AO 1011", unusedFor, true},
		{"ku807", "BK 807 open", "its task is open", false},
		{"go-build-153", "BK 153 done", unusedFor, true},
		{"hf-cache", "program hf-cache", "kept by disk.holders.keep", false},
		{"inuse-holder", "program inuse-holder", "in use by a running process", false},
		{"checkout-holder", "program checkout-holder", "holds a checkout", false},
		{"aside/backstage-3125-second", "unowned (task 3125)", "check its work first", true},
		{"aside/fresh", "program fresh", "written ", false},
		{"moved-aside/kd-3131", "unowned (task 3131)", unusedFor, true},
		{"loose.log", "program loose.log", unusedFor, true},
	} {
		h, ok := got[tc.name]
		if !ok {
			t.Errorf("%s not listed: %+v", tc.name, r.Holders)
			continue
		}
		if h.Owner != tc.owner || h.Eligible != tc.eligible || !strings.Contains(h.Why, tc.why) || h.Bytes == 0 {
			t.Errorf("%s = %+v, want owner %q eligible %v why %q", tc.name, h, tc.owner, tc.eligible, tc.why)
		}
	}
	if _, ok := got["aside"]; ok {
		t.Error("an aside directory is listed as one holder, not by its entries")
	}
	if got["ku807"].Task == "" || !got["ku807"].TaskOpen || got["ao1011"].TaskOpen {
		t.Errorf("tasks: ku807=%+v ao1011=%+v", got["ku807"], got["ao1011"])
	}
	if r.Eligible == 0 || r.Eligible >= r.Bytes || len(r.eligible()) != 6 {
		t.Errorf("eligible %d of %d bytes, %d holders", r.Eligible, r.Bytes, len(r.eligible()))
	}
	for _, p := range []string{"ao1011/notes.md", "aside/backstage-3125-second/.git", "go-build-153/ab/cd"} {
		if !exists(filepath.Join(root, p)) {
			t.Fatalf("listing removed %s", p)
		}
	}
	if s := r.summary(); !strings.Contains(s, "6 holders under "+root+" eligible for removal") || !strings.Contains(s, "nothing removed") {
		t.Errorf("summary=%s", s)
	}
}

func TestStateHoldersWatchSaysEligibleSpaceOnce(t *testing.T) {
	a, root, m := holdersFixture(t)
	a.cfg.Disk.Holders.WarnGiB = 0
	saved := liveHolders
	t.Cleanup(func() { liveHolders = saved })
	calls := 0
	liveHolders = func(*app) (holderMachine, error) { calls++; return m, nil }
	w := &watcher{app: a, holdersOn: true, last: map[string]time.Time{}}
	w.stateHolders(a.now)
	w.bg.Wait()
	if calls != 1 || !w.isActive("stateholders") {
		t.Fatalf("calls=%d active=%v", calls, w.isActive("stateholders"))
	}
	var mark stateHoldersMark
	if _, err := a.store.ReadFile(stateHoldersFile, &mark); err != nil || !mark.On || !strings.Contains(mark.Line, root) {
		t.Fatalf("mark=%+v err=%v", mark, err)
	}
	// Another watch within the interval says the shared listing, walking
	// nothing.
	other := &watcher{app: a, holdersOn: true, last: map[string]time.Time{}}
	other.stateHolders(a.now.Add(time.Minute))
	other.bg.Wait()
	if calls != 1 || !other.isActive("stateholders") {
		t.Fatalf("second watch: calls=%d active=%v", calls, other.isActive("stateholders"))
	}
	// Under the threshold, the condition ends.
	a.cfg.Disk.Holders.WarnGiB = 1 << 20
	w.stateHolders(a.now.Add(7 * time.Hour))
	w.bg.Wait()
	if calls != 2 || w.isActive("stateholders") {
		t.Fatalf("under threshold: calls=%d active=%v", calls, w.isActive("stateholders"))
	}
	a.cfg.Disk.Holders.WarnGiB = -1
	w.stateHolders(a.now.Add(14 * time.Hour))
	w.bg.Wait()
	if calls != 2 {
		t.Fatal("a negative warnGiB still listed the holders")
	}
}

func TestDiskHoldersCommandListsWithoutRemoving(t *testing.T) {
	a, root, m := holdersFixture(t)
	saved := liveHolders
	t.Cleanup(func() { liveHolders = saved })
	liveHolders = func(*app) (holderMachine, error) { return m, nil }
	out := a.out.(interface{ String() string })
	c := a.diskHoldersCmd()
	c.SetArgs([]string{"--min-mib", "0"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{root + ":", "eligible for removal: 6", "OWNER", "ao1011", "AO 1011 (giantswarm/agent-platform#1011)", "yes: unused for", "no: its task is open"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if !exists(filepath.Join(root, "cargo-target-upstream", "debug", "big")) {
		t.Fatal("disk holders removed a holder")
	}
}
