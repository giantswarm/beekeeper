package secret

import (
	"path/filepath"
	"testing"
	"time"
)

func TestKeeperPublishesTransitionsInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault-state.json")
	entered, release := make(chan struct{}), make(chan struct{})
	failures := make(chan error, 2)
	k := NewKeeper(time.Hour, func(st VaultState) {
		if st.Unlocked {
			close(entered)
			<-release
		}
		failures <- WriteState(path, st)
	})
	done := make(chan error, 1)
	go func() { done <- k.Unlock(testSession, testToken, time.Now()) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("unlock callback did not start")
	}
	k.Lock()
	if k.State().Unlocked || k.Env() != "" {
		t.Error("lock did not clear the session")
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("notifications deadlocked")
	}
	for range 2 {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
	}
	st, err := ReadState(path)
	if err != nil || st.Unlocked {
		t.Fatalf("last published state disagrees with the keeper: %+v, %v", st, err)
	}
}

func TestKeeperCallbackCanReadAndLock(t *testing.T) {
	states := make(chan VaultState, 2)
	var k *Keeper
	k = NewKeeper(time.Hour, func(st VaultState) {
		_ = k.State()
		states <- st
		if st.Unlocked {
			k.Lock()
		}
	})
	done := make(chan error, 1)
	go func() { done <- k.Unlock(testSession, testToken, time.Now()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reentrant callback deadlocked")
	}
	if a, b := <-states, <-states; !a.Unlocked || b.Unlocked || k.State().Unlocked {
		t.Fatal("reentrant transitions were published out of order")
	}
}
