package state

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func queueStore(t *testing.T) *FileStore {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func queueTicket(t *testing.T, s *FileStore, shared bool) *lockTicket {
	t.Helper()
	ticket, err := s.enqueueLock(shared)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ticket.leave)
	return ticket
}

func assertTicketReady(t *testing.T, ticket *lockTicket, shared, want bool) {
	t.Helper()
	if got, err := ticket.ready(shared); err != nil || got != want {
		t.Fatalf("ticket %s ready = %v, %v; want %v", ticket.name, got, err, want)
	}
}

func TestStateLockQueueOrdersReadersAndWriters(t *testing.T) {
	s := queueStore(t)
	r1 := queueTicket(t, s, true)
	r2 := queueTicket(t, s, true)
	w1 := queueTicket(t, s, false)
	r3 := queueTicket(t, s, true)
	w2 := queueTicket(t, s, false)
	assertTicketReady(t, r1, true, true)
	assertTicketReady(t, r2, true, true)
	assertTicketReady(t, w1, false, false)
	assertTicketReady(t, r3, true, false)
	assertTicketReady(t, w2, false, false)
	r1.leave()
	r2.leave()
	assertTicketReady(t, w1, false, true)
	// A stream of fresh readers and writers cannot pass the waiting writer.
	for i := range 20 {
		shared := i%2 == 0
		late := queueTicket(t, s, shared)
		assertTicketReady(t, late, shared, false)
		late.leave()
	}
	w1.leave()
	assertTicketReady(t, r3, true, true)
	assertTicketReady(t, w2, false, false)
	r3.leave()
	assertTicketReady(t, w2, false, true)
}

func TestStateLockWriterStopsNewReadersUntilItAcquires(t *testing.T) {
	s := queueStore(t)
	release, err := s.lock(true, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	writer := make(chan error, 1)
	go func() {
		writer <- s.UpdateWithin(5*time.Second, func(st *State) ([]Event, error) {
			st.NextNote++
			return nil, nil
		})
	}()
	waitForStateTickets(t, s, 1)
	// The old lock allowed this read to slip past the waiting writer.
	if unlock, err := s.lock(true, 50*time.Millisecond); err == nil {
		unlock()
		t.Fatal("new reader passed a waiting writer")
	} else {
		var busy *LockBusyError
		if !errors.As(err, &busy) {
			t.Fatal(err)
		}
	}
	release()
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
	st, err := s.Read()
	if err != nil || st.NextNote != 1 {
		t.Fatalf("writer did not commit: %v, %v", st, err)
	}
	waitForStateTickets(t, s, 0)
}

func waitForStateTickets(t *testing.T, s *FileStore, want int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		entries, err := os.ReadDir(s.path(stateQueue))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), ".") {
				n++
			}
		}
		if n == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("queue has %d tickets, want %d", n, want)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestStateLockCanceledTicketDoesNotBlockFollowers(t *testing.T) {
	s := queueStore(t)
	first := queueTicket(t, s, false)
	ctx, cancel := context.WithCancel(t.Context())
	second := queueTicket(t, s, false)
	cancel()
	if err := second.await(ctx, false); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	second.leave()
	third := queueTicket(t, s, false)
	first.leave()
	assertTicketReady(t, third, false, true)
}

func TestStateLockWriterProgressUnderShortReacquirers(t *testing.T) {
	s := queueStore(t)
	release, err := s.lock(false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	const workers = 20
	order := make(chan int, workers+1)
	results := make(chan error, workers+1)
	run := func(id, rounds int, shared bool) {
		// Independent stores exercise the filesystem protocol, not a mutex
		// shared through one FileStore instance.
		other, err := Open(s.Dir())
		if err != nil {
			results <- err
			return
		}
		for i := range rounds {
			unlock, err := other.lock(shared, 10*time.Second)
			if err != nil {
				results <- err
				return
			}
			if i == 0 {
				order <- id
			}
			unlock()
		}
		results <- nil
	}
	go run(0, 1, false)
	waitForStateTickets(t, s, 1)
	for i := range workers {
		go run(i+1, 10, i%2 == 0)
	}
	waitForStateTickets(t, s, workers+1)
	release()
	if first := <-order; first != 0 {
		t.Errorf("short reacquirer %d overtook the waiting writer", first)
	}
	for range workers + 1 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	waitForStateTickets(t, s, 0)
}

// The helper deliberately exits without cleanup when killed. Its ticket lock
// must protect a live waiter in another process and disappear on process death.
func TestStateLockTicketProcess(t *testing.T) {
	if os.Getenv("BEEKEEPER_TEST_LOCK_QUEUE") != "1" {
		return
	}
	// The parent sets the helper's working directory to its temporary store;
	// the environment selects helper mode but supplies no filesystem path.
	s, err := Open(".")
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := s.enqueueLock(false)
	if err != nil {
		t.Fatal(err)
	}
	defer ticket.leave()
	fmt.Println(ticket.name)
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestStateLockQueueRecoversKilledProcess(t *testing.T) {
	s := queueStore(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestStateLockTicketProcess$") //nolint:gosec // the current test executable
	cmd.Dir = s.Dir()
	cmd.Env = append(os.Environ(), "BEEKEEPER_TEST_LOCK_QUEUE=1")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill() }()
	name, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	follower := queueTicket(t, s, false)
	assertTicketReady(t, follower, false, false)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	assertTicketReady(t, follower, false, true)
	if _, err := os.Stat(filepath.Join(s.path(stateQueue), strings.TrimSpace(name))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dead process ticket was not reaped: %v", err)
	}
}

func TestStateLockQueueReadErrorsDoNotBypassOrder(t *testing.T) {
	s := queueStore(t)
	ticket := queueTicket(t, s, false)
	// An unreadable queue must fail, not treat its earlier writers as absent.
	if err := os.Rename(s.path(stateQueue), s.path(stateQueue+".moved")); err != nil {
		t.Fatal(err)
	}
	if err := ticket.await(t.Context(), false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing queue: %v", err)
	}
}
