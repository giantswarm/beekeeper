package takeover

import (
	"testing"
	"time"
)

func TestOwnedReleasePreservesReplacementGeneration(t *testing.T) {
	dir := t.TempDir()
	old := Flag{PID: tScreen, By: "Ada", Since: time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)}
	if err := Take(dir, tSession, old); err != nil {
		t.Fatal(err)
	}
	if err := Release(dir, tSession); err != nil {
		t.Fatal(err)
	}
	next := old
	next.Since = next.Since.Add(time.Second) // Even the same PID needs a new generation.
	if err := Take(dir, tSession, next); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseOwned(dir, tSession, old); err != nil {
		t.Fatal(err)
	}
	if got, ok := Taken(dir, tSession, alive); !ok || !got.Since.Equal(next.Since) {
		t.Fatalf("replacement = %+v, %v", got, ok)
	}
	if err := ReleaseOwned(dir, tSession, next); err != nil {
		t.Fatal(err)
	}
	if _, ok := Taken(dir, tSession, alive); ok {
		t.Fatal("current owner did not release its flag")
	}
	if err := ReleaseOwned(dir, tSession, next); err != nil {
		t.Fatal(err)
	}
}
