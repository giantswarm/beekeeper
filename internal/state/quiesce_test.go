package state

import (
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// An exit that abandons a pass quiesces its writes: a state lock held by
// stuck work costs at most wait, and the commit lock stays held until
// release, so no commit of this process starts before the exit.
func TestQuiesceHoldsCommitsPastABusyStateLock(t *testing.T) {
	s := queueStore(t)
	held, stuck := make(chan struct{}), make(chan struct{})
	updated := make(chan error, 1)
	go func() {
		updated <- s.Update(func(*State) ([]Event, error) {
			close(held)
			<-stuck
			return nil, nil
		})
	}()
	<-held
	start := time.Now()
	release, err := s.Quiesce(50 * time.Millisecond)
	if err != nil {
		t.Fatalf("Quiesce with a busy state lock: %v", err)
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Fatalf("Quiesce waited %s for a busy state lock, want about its 50ms wait", waited)
	}
	commit := flock.New(s.path("state.commit.lock"))
	if ok, err := commit.TryLock(); err != nil || ok {
		t.Fatalf("commit lock free while quiesced (ok %v, err %v)", ok, err)
	}
	release()
	if ok, err := commit.TryLock(); err != nil || !ok {
		t.Fatalf("commit lock still held after release (ok %v, err %v)", ok, err)
	}
	_ = commit.Unlock()
	close(stuck)
	if err := <-updated; err != nil {
		t.Fatalf("update after release: %v", err)
	}
}

// With no writer, Quiesce holds the state lock too, so no append starts.
func TestQuiesceHoldsAFreeStateLock(t *testing.T) {
	s := queueStore(t)
	release, err := s.Quiesce(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	state := flock.New(s.path("state.lock"))
	if ok, err := state.TryLock(); err != nil || ok {
		t.Fatalf("state lock free while quiesced (ok %v, err %v)", ok, err)
	}
	release()
	if ok, err := state.TryLock(); err != nil || !ok {
		t.Fatalf("state lock still held after release (ok %v, err %v)", ok, err)
	}
	_ = state.Unlock()
}
