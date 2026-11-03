package guard

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/platform"
)

// syncCapper runs the command uncapped, safe for runs in parallel.
type syncCapper struct{}

func (syncCapper) Available() bool                       { return true }
func (syncCapper) Capped() bool                          { return false }
func (syncCapper) Adopt(int, string, platform.Cap) error { return nil }
func (syncCapper) CapSlot(platform.Cap) error            { return nil }
func (syncCapper) Reap(string) ([]int, error)            { return nil, nil }
func (syncCapper) Command(_ string, _ platform.Cap, argv []string) (*exec.Cmd, error) {
	return exec.Command(argv[0], argv[1:]...), nil //nolint:gosec // the test's own command
}

// fastPoll shortens the waiters' poll for a test.
func fastPoll(t *testing.T) {
	t.Helper()
	saved := slotPoll
	slotPoll = 20 * time.Millisecond
	t.Cleanup(func() { slotPoll = saved })
}

// A start that builds, competing with two runners that take the only slot
// one after the other without a gap, gets its turn in the order it came:
// at most the runs queued ahead of it go first, never the runners'
// next runs after it.
func TestAwaitSlotTakesItsTurnBetweenAlternatingRuns(t *testing.T) {
	saved := plat.Capper
	plat.Capper = syncCapper{}
	t.Cleanup(func() { plat.Capper = saved })
	fastPoll(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")

	var mu sync.Mutex
	starts := 0
	o := Options{Max: "1M", Swap: "0", Wait: 20 * time.Second, SlotDir: t.TempDir(), Slots: 1, Stderr: io.Discard,
		Record: func(verb, _ string) {
			if verb == VerbStart {
				mu.Lock()
				starts++
				mu.Unlock()
			}
		}}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if rc := Run(o, []string{"sleep", "0.2"}); rc != 0 {
					t.Errorf("a runner's run: exit %d", rc)
					return
				}
			}
		})
	}
	t.Cleanup(func() { close(stop); wg.Wait() })

	// both runners hold or wait for the slot before the start comes
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := starts
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the runners never alternated in the slot")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	before := starts
	mu.Unlock()
	w := o
	w.Wait = 5 * time.Second
	ok, ahead, err := AwaitSlot(w, "agents start BK 1")
	mu.Lock()
	between := starts - before
	mu.Unlock()
	if err != nil || !ok {
		t.Fatalf("the start's turn never came: ok %v, %d ahead, %v", ok, ahead, err)
	}
	if between > 2 {
		t.Errorf("%d runs took the slot while the start waited, want at most the 2 queued ahead of it", between)
	}
}

// A start whose turn does not come within its wait says how many waited
// ahead of it, and leaves the queue.
func TestAwaitSlotGivesUpWithTheQueueAhead(t *testing.T) {
	fastPoll(t)
	dir := t.TempDir()
	holdSlot(t, dir, 1, "other")
	ahead, err := enqueue(dir, "run 1: go test ./...")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ahead.leave)
	var stderr strings.Builder
	o := Options{SlotDir: dir, Slots: 1, Wait: 100 * time.Millisecond, Stderr: &stderr}
	ok, n, err := AwaitSlot(o, "agents start BK 2")
	if err != nil || ok || n != 1 {
		t.Fatalf("AwaitSlot = %v, %d ahead, %v: want no turn, 1 ahead", ok, n, err)
	}
	if !strings.Contains(stderr.String(), "1. run 1: go test ./...") {
		t.Errorf("the wait does not name the waiter ahead:\n%s", stderr.String())
	}
	entries, _ := os.ReadDir(queueDir(dir))
	if len(entries) != 1 {
		t.Errorf("the queue holds %d tickets after the start left, want the 1 ahead", len(entries))
	}
}

// A ticket whose waiter ended without leaving (its lock free) holds nobody
// up, and the next look removes it.
func TestEndedWaitersTicketIsRemoved(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(queueDir(dir), 0o750); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(queueDir(dir), "00000000000000000001-4711")
	if err := os.WriteFile(stale, []byte("run 4711: make\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tk, err := enqueue(dir, "agents start BK 3")
	if err != nil {
		t.Fatal(err)
	}
	defer tk.leave()
	if turn, why := tk.turn(1); !turn {
		t.Errorf("an ended waiter's ticket holds the start up: %s", why)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the ended waiter's ticket is still there: %v", err)
	}
}

// A peek at the slots judges a start that builds as its wait would, and
// takes neither a ticket nor a slot: free while a slot is free and fewer
// waiters than slots are queued, else the holders and the waiters named.
func TestPeekSlotJudgesWithoutClaiming(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "slots")
	o := Options{SlotDir: dir, Slots: 1}
	if free, why, err := PeekSlot(o); !free || why != "" || err != nil {
		t.Fatalf("PeekSlot on a free slot = %v %q %v, want free", free, why, err)
	}
	if lock, _, _ := acquire(o); lock == nil {
		t.Fatal("the peek left the free slot held")
	} else {
		_ = lock.Unlock()
	}
	holdSlot(t, dir, 1, "other")
	free, why, _ := PeekSlot(o)
	if free || !strings.Contains(why, "all 1 held") || !strings.Contains(why, "agentlab backstage-test") {
		t.Errorf("PeekSlot on a held slot = %v %q, want busy with its holder", free, why)
	}
	waiter, err := enqueue(dir, "run 1: go test ./...")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(waiter.leave)
	if free, why, _ := PeekSlot(o); free || !strings.Contains(why, "1. run 1: go test ./...") {
		t.Errorf("PeekSlot behind a waiter = %v %q, want busy with the waiter ahead", free, why)
	}
	if entries, _ := os.ReadDir(queueDir(dir)); len(entries) != 1 {
		t.Errorf("the queue holds %d tickets after the peeks, want the 1 waiter", len(entries))
	}
}

// A free slot is no turn while as many waiters as slots are queued: they go
// first.
func TestPeekSlotQueuesBehindTheWaiters(t *testing.T) {
	dir := t.TempDir()
	waiter, err := enqueue(dir, "agents start BK 4")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(waiter.leave)
	free, why, _ := PeekSlot(Options{SlotDir: dir, Slots: 1})
	if free || strings.Contains(why, "held") || !strings.Contains(why, "1. agents start BK 4") {
		t.Errorf("PeekSlot = %v %q, want busy behind the waiter, no slot held", free, why)
	}
}
