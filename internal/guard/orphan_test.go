package guard

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/platform"
)

type orphanCapper struct {
	fakeCapper
	populated func() (bool, error)
	argv      []string
}

func (f *orphanCapper) ScopePopulated(string) (bool, error) { return f.populated() }
func (f *orphanCapper) Command(string, platform.Cap, []string) (*exec.Cmd, error) {
	if f.argv != nil {
		return exec.Command(f.argv[0], f.argv[1:]...), nil //nolint:gosec // the test's own command
	}
	return exec.Command("sleep", "2"), nil
}

func TestRunReleasesOrphanedScope(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(map[bool]string{false: "never-started", true: "ended"}[started], func(t *testing.T) {
			saved := plat.Capper
			checks := 0
			f := &orphanCapper{populated: func() (bool, error) { checks++; return started && checks == 1, nil }}
			plat.Capper = f
			t.Cleanup(func() { plat.Capper = saved })
			oldPoll, oldStart := scopePoll, scopeStartWait
			scopePoll, scopeStartWait = time.Millisecond, 10*time.Millisecond
			t.Cleanup(func() { scopePoll, scopeStartWait = oldPoll, oldStart })
			var log bytes.Buffer
			o := Options{Max: "1M", SlotDir: t.TempDir(), Slots: 1, Stderr: &log}
			start := time.Now()
			rc := Run(o, []string{"ignored"})
			if rc != 1 || time.Since(start) > time.Second {
				t.Fatalf("orphan exit %d after %s; log %s", rc, time.Since(start), log.String())
			}
			if !strings.Contains(log.String(), "no live processes") {
				t.Fatalf("missing orphan report: %s", log.String())
			}
			lock, _, _ := acquire(o)
			if lock == nil {
				t.Fatal("orphan kept its slot")
			}
			_ = lock.Unlock()
		})
	}
}

func TestScopeWatchdogPreservesLiveAndUnreadableCommands(t *testing.T) {
	for _, mode := range []string{"live", "unreadable", "starting"} {
		t.Run(mode, func(t *testing.T) {
			f := &orphanCapper{argv: []string{"sh", "-c", "sleep 0.03; exit 23"}, populated: func() (bool, error) {
				if mode == "unreadable" {
					return false, fmt.Errorf("cannot read scope")
				}
				return mode == "live", nil
			}}
			saved := plat.Capper
			plat.Capper = f
			t.Cleanup(func() { plat.Capper = saved })
			oldPoll, oldStart := scopePoll, scopeStartWait
			scopePoll, scopeStartWait = time.Millisecond, time.Second
			t.Cleanup(func() { scopePoll, scopeStartWait = oldPoll, oldStart })
			if rc := scope(f, "test", Options{Stderr: io.Discard}, 1, nil, nil, func(string, ...any) {}); rc != 23 {
				t.Fatalf("exit %d, want 23", rc)
			}
		})
	}
}

func TestQueueReportsEmptyHolderScope(t *testing.T) {
	f := &orphanCapper{populated: func() (bool, error) { return false, nil }}
	saved := plat.Capper
	plat.Capper = f
	t.Cleanup(func() { plat.Capper = saved })
	o := Options{SlotDir: t.TempDir(), Slots: 1}
	holdSlot(t, o.SlotDir, 1, "test")
	raw := fmt.Sprintf(`{"pid":1,"scope":"memcap-test","since":%q}`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(o.SlotDir, "1.holder"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if got := holders(o); !strings.Contains(got, "no live processes in memcap-test.scope") {
		t.Fatalf("missing queue warning: %s", got)
	}
}

func TestRunReleasesSlotWhenCommandCannotStart(t *testing.T) {
	saved := plat.Capper
	plat.Capper = &fakeCapper{}
	t.Cleanup(func() { plat.Capper = saved })
	o := Options{Max: "1M", SlotDir: t.TempDir(), Slots: 1, Stderr: io.Discard}
	if rc := Run(o, []string{filepath.Join(t.TempDir(), "missing-command")}); rc != ExitNotFound {
		t.Fatalf("exit %d, want %d", rc, ExitNotFound)
	}
	lock, _, _ := acquire(o)
	if lock == nil {
		t.Fatal("failed start retained slot")
	}
	_ = lock.Unlock()
	if _, err := os.Stat(filepath.Join(o.SlotDir, "1.holder")); !os.IsNotExist(err) {
		t.Fatalf("failed start retained holder: %v", err)
	}
}

func TestMemoryWaitHoldsNoBuildSlot(t *testing.T) {
	saved := plat.Machine
	plat.Machine = lowMem{Machine: saved}
	t.Cleanup(func() { plat.Machine = saved })
	o := Options{SlotDir: t.TempDir(), Slots: 1, Wait: time.Millisecond, Stderr: io.Discard}
	checked := false
	logf := func(_ string, _ ...any) {
		checked = true
		if ahead := waiters(queueDir(o.SlotDir), ""); len(ahead) != 0 {
			t.Error("memory waiter blocks the slot queue")
		}
		lock, _, _ := acquire(o)
		if lock == nil {
			t.Error("memory waiter holds a build slot")
		} else {
			_ = lock.Unlock()
		}
	}
	lock, slot := waitForSlot(o, 8<<20, "", []string{"ignored"}, logf)
	if lock != nil || slot != 0 || !checked {
		t.Fatalf("lock %v slot %d checked %v", lock, slot, checked)
	}
}

func TestMemoryShortWaiterRetainsTicketWhileSlotsBusy(t *testing.T) {
	saved := plat.Machine
	plat.Machine = lowMem{Machine: saved}
	t.Cleanup(func() { plat.Machine = saved })
	o := Options{SlotDir: t.TempDir(), Slots: 1, Wait: time.Millisecond, Stderr: io.Discard}
	holdSlot(t, o.SlotDir, 1, "other")
	checked := false
	logf := func(string, ...any) {
		checked = true
		if a := waiters(queueDir(o.SlotDir), ""); len(a) != 1 {
			t.Errorf("busy-slot waiter lost ticket: %v", a)
		}
	}
	lock, slot := waitForSlot(o, 8<<20, "", []string{"first"}, logf)
	if lock != nil || slot != 0 || !checked {
		t.Fatalf("unexpected result %v %d %v", lock, slot, checked)
	}
}

func TestMemoryShortWaiterRetainsTicketBeforeItsTurn(t *testing.T) {
	saved := plat.Machine
	plat.Machine = lowMem{Machine: saved}
	t.Cleanup(func() { plat.Machine = saved })
	o := Options{SlotDir: t.TempDir(), Slots: 1, Wait: time.Millisecond, Stderr: io.Discard}
	first, err := enqueue(o.SlotDir, "first")
	if err != nil {
		t.Fatal(err)
	}
	defer first.leave()
	logf := func(string, ...any) {
		if a := waiters(queueDir(o.SlotDir), ""); len(a) != 2 {
			t.Errorf("waiter before its turn lost ticket: %v", a)
		}
	}
	lock, _ := waitForSlot(o, 8<<20, "", []string{"second"}, logf)
	if lock != nil {
		_ = lock.Unlock()
		t.Fatal("second waiter acquired slot")
	}
}
