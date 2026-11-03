package gocache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// cache writes a cache of entries name → (size, age) under a fresh
// directory, with the files the scan leaves out beside them.
func cache(t *testing.T, entries map[string][2]int) string {
	t.Helper()
	dir := t.TempDir()
	now := time.Now()
	for name, e := range entries {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, e[0]), 0o600); err != nil {
			t.Fatal(err)
		}
		at := now.Add(-time.Duration(e[1]) * time.Hour)
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	for _, other := range []string{"README", "trim.txt", "fuzz/x/corpus"} {
		p := filepath.Join(dir, other)
		_ = os.MkdirAll(filepath.Dir(p), 0o750)
		if err := os.WriteFile(p, make([]byte, 1000), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The entries of the trim tests, newest first.
const entryA, entryB, entryC, entryD = "00/a", "01/b", "02/c", "03/d"

func TestScanOrdersLeastRecentlyUsedFirst(t *testing.T) {
	dir := cache(t, map[string][2]int{"00/new-a": {10, 1}, "ff/old-d": {20, 48}, "7a/mid-d": {30, 5}})
	u, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if u.Bytes != 60 || len(u.Entries) != 3 {
		t.Fatalf("scan = %d bytes in %d entries, want 60 in 3 (README, trim.txt and fuzz left out)", u.Bytes, len(u.Entries))
	}
	for i, want := range []string{"old-d", "mid-d", "new-a"} {
		if got := filepath.Base(u.Entries[i].Path); got != want {
			t.Errorf("entry %d = %s, want %s", i, got, want)
		}
	}
}

func TestScanMissingCacheIsEmpty(t *testing.T) {
	u, err := Scan(filepath.Join(t.TempDir(), "absent"))
	if err != nil || u.Bytes != 0 || len(u.Entries) != 0 {
		t.Fatalf("scan = %+v, %v; want empty", u, err)
	}
}

func TestTrimRemovesOldestDownToTarget(t *testing.T) {
	dir := cache(t, map[string][2]int{entryA: {100, 1}, entryB: {100, 10}, entryC: {100, 20}, entryD: {100, 30}})
	u, _ := Scan(dir)
	freed, removed, err := Trim(u, 250, func() bool { return false })
	if err != nil || freed != 200 || removed != 2 {
		t.Fatalf("trim = %d bytes, %d entries, %v; want 200, 2, nil", freed, removed, err)
	}
	for name, kept := range map[string]bool{entryA: true, entryB: true, entryC: false, entryD: false} {
		if _, err := os.Stat(filepath.Join(dir, name)); (err == nil) != kept {
			t.Errorf("%s kept = %v, want %v", name, err == nil, kept)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "README")); err != nil {
		t.Error("README removed")
	}
}

func TestTrimUnderTargetRemovesNothing(t *testing.T) {
	dir := cache(t, map[string][2]int{entryA: {100, 100}})
	u, _ := Scan(dir)
	if freed, removed, err := Trim(u, 100, func() bool { return true }); freed != 0 || removed != 0 || err != nil {
		t.Fatalf("trim = %d, %d, %v; want nothing", freed, removed, err)
	}
}

func TestTrimStopsWhileABuildRuns(t *testing.T) {
	dir := cache(t, map[string][2]int{entryA: {100, 1}, entryB: {100, 10}})
	u, _ := Scan(dir)
	_, removed, err := Trim(u, 0, func() bool { return true })
	if !errors.Is(err, ErrBusy) || removed != 0 {
		t.Fatalf("trim = %d removed, %v; want 0, ErrBusy", removed, err)
	}
}

func TestTrimSkipsAnEntryGoneAlready(t *testing.T) {
	dir := cache(t, map[string][2]int{entryA: {100, 1}, entryB: {100, 10}, entryC: {100, 20}})
	u, _ := Scan(dir)
	if err := os.Remove(filepath.Join(dir, entryC)); err != nil {
		t.Fatal(err)
	}
	freed, removed, err := Trim(u, 150, func() bool { return false })
	if err != nil || freed != 100 || removed != 1 {
		t.Fatalf("trim = %d, %d, %v; want 100, 1, nil", freed, removed, err)
	}
}

func TestDir(t *testing.T) {
	t.Setenv("GOCACHE", "/x/go-build")
	if d := Dir(); d != "/x/go-build" {
		t.Errorf("Dir = %q", d)
	}
	t.Setenv("GOCACHE", "off")
	if d := Dir(); d != "" {
		t.Errorf("Dir with GOCACHE=off = %q, want \"\"", d)
	}
}

// A trim waits for the commands that use the cache, and a command for the
// trim under way.
func TestUseAndTrimExcludeEachOther(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "go-build")
	a, err := Use(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Use(context.Background(), dir)
	if err != nil {
		t.Fatal("a second command waited for the first:", err)
	}
	if _, ok, err := TryTrim(dir); ok || err != nil {
		t.Fatalf("a trim began while two commands used the cache: %v, %v", ok, err)
	}
	a()
	if _, ok, _ := TryTrim(dir); ok {
		t.Fatal("a trim began while a command used the cache")
	}
	b()
	release, ok, err := TryTrim(dir)
	if !ok || err != nil {
		t.Fatalf("no trim once the commands ended: %v, %v", ok, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := Use(ctx, dir); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a command used the cache under a trim: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		r, err := Use(context.Background(), dir)
		if err == nil {
			r()
		}
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the command still waits after the trim ended")
	}
}

// A clear waits for the runs that hold the cache, and a run that starts
// while it waits waits behind it, so overlapping runs cannot keep it
// waiting.
func TestHoldWaitsForRunsAndHoldsBackNewOnes(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	run, err := Use(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	held := make(chan func())
	go func() {
		release, err := Hold(ctx, dir, func() { close(waited) })
		if err != nil {
			t.Error(err)
		}
		held <- release
	}()
	<-waited
	late, cancel := context.WithTimeout(ctx, 3*lockRetry)
	defer cancel()
	if _, err := Use(late, dir); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a run that started while the clear waited took the cache: %v", err)
	}
	select {
	case <-held:
		t.Fatal("the clear held the cache while a run held it")
	case <-time.After(2 * lockRetry):
	}
	run()
	release := <-held
	if _, ok, _ := TryTrim(dir); ok {
		t.Error("a trim took the cache the clear holds")
	}
	release()
	next, err := Use(ctx, dir)
	if err != nil {
		t.Fatalf("a run after the clear: %v", err)
	}
	next()
}

func TestHoldWithoutRunsDoesNotWait(t *testing.T) {
	release, err := Hold(context.Background(), t.TempDir(), func() { t.Error("waited with no run") })
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestHoldGivesUpWithItsContext(t *testing.T) {
	dir := t.TempDir()
	run, err := Use(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer run()
	ctx, cancel := context.WithTimeout(context.Background(), 2*lockRetry)
	defer cancel()
	if _, err := Hold(ctx, dir, func() {}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hold = %v, want the deadline", err)
	}
	// The turnstile is given back: a run goes on.
	again, err := Use(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	again()
}

func TestModDir(t *testing.T) {
	t.Setenv("GOMODCACHE", "/m")
	if got := ModDir(); got != "/m" {
		t.Errorf("GOMODCACHE: %s", got)
	}
	t.Setenv("GOMODCACHE", "")
	t.Setenv("GOPATH", "/p1"+string(os.PathListSeparator)+"/p2")
	if got := ModDir(); got != "/p1/pkg/mod" {
		t.Errorf("GOPATH: %s", got)
	}
	t.Setenv("GOPATH", "")
	t.Setenv("HOME", "/h")
	if got := ModDir(); got != "/h/go/pkg/mod" {
		t.Errorf("home: %s", got)
	}
}
