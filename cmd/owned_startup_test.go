//go:build unix

package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/guard"
)

func TestWaitFollowerGetsLeaderStartupFailure(t *testing.T) {
	noSystemd(t)
	fakeDevctl(t, "exit 91")
	a := queueApp(t)
	argv := strings.Fields("devctl pr wait o/r 7")
	lead := ownedBase(a.store.Dir(), argv, os.Getpid())
	follower := filepath.Join(a.store.Dir(), ownedRuns, "startup-follower")
	wasSelf := selfExe
	t.Cleanup(func() { selfExe = wasSelf })
	attached := false
	selfExe = func() (string, error) {
		selected, polls := claimLead(a.store.Dir(), argv, follower)
		attached = !polls && selected == lead
		return "", errors.New("fixture launcher unavailable")
	}
	if err := a.ownedRun(context.Background(), argv, 0); Code(err) != guard.ExitNotFound {
		t.Fatalf("leader startup exit=%d, want %d", Code(err), guard.ExitNotFound)
	}
	if !attached {
		t.Fatal("follower did not attach before leader startup failed")
	}
	done := make(chan int, 1)
	go func() { done <- followRun(lead, io.Discard, io.Discard) }()
	select {
	case rc := <-done:
		if rc != guard.ExitNotFound {
			t.Fatalf("follower exit=%d, want leader's %d", rc, guard.ExitNotFound)
		}
	case <-time.After(time.Second):
		// End only the test's reader before failing; main otherwise waits
		// forever because neither a PID nor a result was written.
		writeResult(lead, runResult{RC: 0})
		<-done
		t.Fatal("follower never received the leader's startup failure")
	}
}
