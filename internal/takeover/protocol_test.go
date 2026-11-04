package takeover

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

const protocolCancel = "cancel"

const protocolAnswer = "answer"

const protocolRelease = "release"

func protocolFixture(t *testing.T) (string, Request, Flag) {
	t.Helper()
	dir := t.TempDir()
	r := Request{Session: tSession, ID: "protocol-request"}
	f := Flag{PID: tScreen, By: "screen", Since: time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)}
	if err := Take(dir, r.Session, f); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(dir, r.Session, r.ID+requestExt), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	return dir, r, f
}

func assertNoHeldFiles(t *testing.T, dir string, r Request) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, r.Session))
	if errors.Is(err, os.ErrNotExist) {
		return // the empty request folder is removed with its last file
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("finished hold left request, answer or temporary files: %v", entries)
	}
}

func TestHeldDecisionRejectsInvalidationBeforeAnswer(t *testing.T) {
	for _, cause := range []string{protocolRelease, "dead screen", protocolCancel, "replacement"} {
		t.Run(cause, func(t *testing.T) {
			dir, r, owner := protocolFixture(t)
			if err := Answer(dir, r.Session, r.ID, Decision{Behavior: Allow}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			live := alive
			switch cause {
			case protocolRelease:
				if err := Release(dir, r.Session); err != nil {
					t.Fatal(err)
				}
			case "dead screen":
				live = func(int) bool { return false }
			case protocolCancel:
				cancel()
			case "replacement":
				next := owner
				next.Since = next.Since.Add(time.Second)
				if err := Take(dir, r.Session, next); err != nil {
					t.Fatal(err)
				}
			}
			d, ok, done, err := inspectHeld(ctx, dir, r, owner, live)
			if err != nil || ok || !done || d != (Decision{}) {
				t.Fatalf("invalidated decision = %+v, %v, %v, %v", d, ok, done, err)
			}
			assertNoHeldFiles(t, dir, r)
		})
	}
}

func TestAnswerAfterReleaseIsHandedBack(t *testing.T) {
	dir, r, owner := protocolFixture(t)
	if err := Release(dir, r.Session); err != nil {
		t.Fatal(err)
	}
	if err := Answer(dir, r.Session, r.ID, Decision{Behavior: Allow}); !errors.Is(err, ErrHandedBack) {
		t.Fatalf("late answer = %v", err)
	}
	if _, ok, done, err := inspectHeld(context.Background(), dir, r, owner, alive); err != nil || ok || !done {
		t.Fatalf("released hold = %v, %v, %v", ok, done, err)
	}
	assertNoHeldFiles(t, dir, r)
}

func TestAnswerPublicationAndFinalizationShareLock(t *testing.T) {
	dir, r, owner := protocolFixture(t)
	called := false
	err := answer(dir, r.Session, r.ID, Decision{Behavior: Deny}, func(path string, b []byte) error {
		called = true
		competitor := flock.New(filepath.Join(dir, ".locks", r.Session+".lock"))
		acquired, err := competitor.TryLock()
		if err != nil {
			t.Fatal(err)
		}
		if acquired {
			_ = competitor.Unlock()
			t.Fatal("finalization could enter between the request check and answer publication")
		}
		return writeAtomic(path, b)
	})
	if err != nil || !called {
		t.Fatalf("publish = %v, called %v", err, called)
	}
	d, ok, done, err := inspectHeld(context.Background(), dir, r, owner, alive)
	if err != nil || !ok || !done || d.Behavior != Deny {
		t.Fatalf("consumption = %+v, %v, %v, %v", d, ok, done, err)
	}
	called = false
	err = answer(dir, r.Session, r.ID, Decision{Behavior: Allow}, func(string, []byte) error { called = true; return nil })
	if !errors.Is(err, ErrHandedBack) || called {
		t.Fatalf("answer after finalization = %v, publish called %v", err, called)
	}
	assertNoHeldFiles(t, dir, r)
}

func TestHoldSynchronizedLifecycle(t *testing.T) {
	for _, action := range []string{protocolAnswer, protocolRelease, protocolCancel} {
		t.Run(action, func(t *testing.T) {
			dir := t.TempDir()
			r := Request{Session: tSession, ID: "held-request"}
			if err := Take(dir, r.Session, Flag{PID: tScreen}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			ready := make(chan struct{})
			done := make(chan [3]any, 1)
			go func() {
				calls := 0
				d, ok, err := Hold(ctx, dir, r, func(pid int) bool {
					calls++
					if calls == 2 {
						close(ready)
					}
					return alive(pid)
				}, time.Millisecond)
				done <- [3]any{d, ok, err}
			}()
			select {
			case <-ready:
			case <-ctx.Done():
				t.Fatal("hold did not publish its request")
			}
			switch action {
			case protocolAnswer:
				if err := Answer(dir, r.Session, r.ID, Decision{Behavior: Deny}); err != nil {
					t.Fatal(err)
				}
			case protocolRelease:
				if err := Release(dir, r.Session); err != nil {
					t.Fatal(err)
				}
			case protocolCancel:
				cancel()
			}
			select {
			case got := <-done:
				if got[2] != nil || got[1].(bool) != (action == protocolAnswer) {
					t.Fatalf("Hold = %v", got)
				}
				if action == protocolAnswer && got[0].(Decision).Behavior != Deny {
					t.Fatalf("decision = %v", got[0])
				}
			case <-time.After(3 * time.Second):
				t.Fatal("hold did not finish")
			}
			assertNoHeldFiles(t, dir, r)
		})
	}
}
