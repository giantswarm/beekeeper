package takeover

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

const (
	tSession = "5f0c1d2e-0000-4000-8000-000000000001"
	tScreen  = 4242
)

func alive(pid int) bool { return pid == tScreen }

func TestTakenNeedsTheFlagAndItsScreen(t *testing.T) {
	dir := t.TempDir()
	if _, ok := Taken(dir, tSession, alive); ok {
		t.Fatal("a session without a flag is taken over")
	}
	if err := Take(dir, tSession, Flag{PID: tScreen, By: "Ada"}); err != nil {
		t.Fatal(err)
	}
	if f, ok := Taken(dir, tSession, alive); !ok || f.By != "Ada" {
		t.Fatalf("Taken = %+v, %v after Take", f, ok)
	}
	if _, ok := Taken(dir, tSession, func(int) bool { return false }); ok {
		t.Error("a flag whose screen is gone still takes the session over")
	}
	if err := Release(dir, tSession); err != nil {
		t.Fatal(err)
	}
	if _, ok := Taken(dir, tSession, alive); ok {
		t.Error("a released session is still taken over")
	}
	if err := Release(dir, tSession); err != nil {
		t.Errorf("a second release = %v, want none", err)
	}
	for _, bad := range []string{"", "..", "a/b"} {
		if err := Take(dir, bad, Flag{PID: tScreen}); err == nil {
			t.Errorf("Take(%q) wrote a flag", bad)
		}
	}
}

// hold runs Hold in the background and returns its outcome's channel.
func hold(ctx context.Context, dir string, r Request) <-chan [3]any {
	out := make(chan [3]any, 1)
	go func() {
		d, ok, err := Hold(ctx, dir, r, alive, 5*time.Millisecond)
		out <- [3]any{d, ok, err}
	}()
	return out
}

// waitPending waits until the screen sees the held request.
func waitPending(t *testing.T, dir string) Request {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rs, _ := Pending(dir, tSession); len(rs) == 1 {
			return rs[0]
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the held request never showed as pending")
	return Request{}
}

func TestHoldGetsTheScreensAnswer(t *testing.T) {
	dir := t.TempDir()
	if err := Take(dir, tSession, Flag{PID: tScreen}); err != nil {
		t.Fatal(err)
	}
	done := hold(context.Background(), dir, Request{ID: "r1", Session: tSession, Tool: "Bash", At: time.Now()})
	r := waitPending(t, dir)
	if r.Tool != "Bash" {
		t.Errorf("pending = %+v", r)
	}
	if err := Answer(dir, tSession, r.ID, Decision{Behavior: Deny, Message: "not now"}); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if d, ok := got[0].(Decision), got[1].(bool); !ok || d.Behavior != Deny || d.Message != "not now" {
		t.Fatalf("Hold = %+v, %v, want the screen's deny", d, ok)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, tSession, "*")); len(left) != 0 {
		t.Errorf("Hold left %v behind", left)
	}
	if err := Answer(dir, tSession, r.ID, Decision{Behavior: Allow}); !errors.Is(err, ErrHandedBack) {
		t.Errorf("a late answer = %v, want ErrHandedBack", err)
	}
}

func TestHoldHandsBack(t *testing.T) {
	dir := t.TempDir()
	if err := Take(dir, tSession, Flag{PID: tScreen}); err != nil {
		t.Fatal(err)
	}

	// The give-up: the caller's deadline ends the hold with no decision.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	got := <-hold(ctx, dir, Request{ID: "r2", Session: tSession, At: time.Now()})
	if got[1].(bool) || got[2] != nil {
		t.Fatalf("Hold past its deadline = %v, want no decision", got)
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("the give-up took %s", el)
	}

	// The release: the screen lets go while a request is held.
	done := hold(context.Background(), dir, Request{ID: "r3", Session: tSession, At: time.Now()})
	waitPending(t, dir)
	if err := Release(dir, tSession); err != nil {
		t.Fatal(err)
	}
	if got := <-done; got[1].(bool) {
		t.Fatalf("Hold after the release = %v, want no decision", got)
	}
	if rs, _ := Pending(dir, tSession); len(rs) != 0 {
		t.Errorf("a handed-back request is still pending: %+v", rs)
	}
}

func TestHoldIgnoresAGarbledAnswer(t *testing.T) {
	dir := t.TempDir()
	if err := Take(dir, tSession, Flag{PID: tScreen}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := hold(ctx, dir, Request{ID: "r4", Session: tSession, At: time.Now()})
	waitPending(t, dir)
	if err := os.WriteFile(filepath.Join(dir, tSession, "r4"+answerExt), []byte(`{"behavior":"maybe"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := <-done; got[1].(bool) {
		t.Fatalf("a garbled answer decided: %v", got)
	}
}

// A release must reject an answer even while the request awaits its next poll.
func TestAnswerAfterReleaseBeforeHoldPoll(t *testing.T) {
	dir := t.TempDir()
	if err := Take(dir, tSession, Flag{PID: tScreen}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		_, ok, _ := Hold(ctx, dir, Request{ID: "late", Session: tSession}, alive, time.Hour)
		done <- ok
	}()
	waitPending(t, dir)
	if err := Release(dir, tSession); err != nil {
		t.Fatal(err)
	}
	if err := Answer(dir, tSession, "late", Decision{Behavior: Allow}); !errors.Is(err, ErrHandedBack) {
		t.Fatalf("late answer = %v", err)
	}
	// Retaking with the same screen must not revive the old request.
	if err := Take(dir, tSession, Flag{PID: tScreen}); err != nil {
		t.Fatal(err)
	}
	if err := Answer(dir, tSession, "late", Decision{Behavior: Allow}); !errors.Is(err, ErrHandedBack) {
		t.Fatalf("answer from a new takeover = %v", err)
	}
	cancel()
	if <-done {
		t.Fatal("released hold accepted an answer")
	}
}

func TestHoldChecksValidityBeforeAnswer(t *testing.T) {
	for _, reason := range []string{"release", "replacement", "dead screen", "cancellation"} {
		t.Run(reason, func(t *testing.T) {
			dir := t.TempDir()
			if err := Take(dir, tSession, Flag{PID: tScreen}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var live atomic.Bool
			live.Store(true)
			done := make(chan [3]any, 1)
			go func() {
				d, ok, err := Hold(ctx, dir, Request{ID: "validity", Session: tSession}, func(int) bool { return live.Load() }, 100*time.Millisecond)
				done <- [3]any{d, ok, err}
			}()
			waitPending(t, dir)
			// Block the next poll while arranging an answer and an invalidation.
			unlock, err := lockSession(dir, tSession, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeAtomic(filepath.Join(dir, tSession, "validity"+answerExt), []byte(`{"behavior":"allow"}`)); err != nil {
				t.Fatal(err)
			}
			switch reason {
			case "release":
				err = os.Remove(filepath.Join(dir, tSession+flagExt))
			case "replacement":
				b, marshalErr := json.Marshal(Flag{PID: tScreen, Generation: "replacement"})
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				err = writeAtomic(filepath.Join(dir, tSession+flagExt), b)
			case "dead screen":
				live.Store(false)
			case "cancellation":
				cancel()
			}
			unlock()
			if err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-done:
				if got[1].(bool) || got[2] != nil {
					t.Fatalf("invalidated hold = %v", got)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("hold did not finish")
			}
			if paths, _ := filepath.Glob(filepath.Join(dir, tSession, "*")); len(paths) != 0 {
				t.Fatalf("finished files: %v", paths)
			}
		})
	}
}

func TestAnswerWaitsForFinalization(t *testing.T) {
	dir := t.TempDir()
	if err := Take(dir, tSession, Flag{PID: tScreen}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := hold(ctx, dir, Request{ID: "final", Session: tSession})
	waitPending(t, dir)
	unlock, err := lockSession(dir, tSession, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	answer := make(chan error, 1)
	go func() { answer <- Answer(dir, tSession, "final", Decision{Behavior: Allow}) }()
	select {
	case err := <-answer:
		t.Fatalf("answer bypassed transition lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	// Model Hold's finalization while publication waits on the same file lock.
	for _, ext := range []string{requestExt, answerExt} {
		if err := removeFinished(filepath.Join(dir, tSession, "final"+ext)); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	unlock()
	if err := <-answer; !errors.Is(err, ErrHandedBack) {
		t.Fatalf("answer after cleanup = %v", err)
	}
	<-done
	if paths, _ := filepath.Glob(filepath.Join(dir, tSession, "*")); len(paths) != 0 {
		t.Fatalf("orphan files: %v", paths)
	}
}

func TestSessionLockIsBoundedAndIndependent(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockSession(dir, tSession, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if release, err := lockSession(dir, tSession, 20*time.Millisecond); err == nil {
		release()
		t.Fatal("contended lock succeeded")
	}
	if err := Take(dir, "another-session", Flag{PID: tScreen}); err != nil {
		t.Fatalf("unrelated session blocked: %v", err)
	}
}
