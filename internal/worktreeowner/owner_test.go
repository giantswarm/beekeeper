package worktreeowner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeOwnerLifecycle(t *testing.T) {
	state := t.TempDir()
	dir := filepath.Join(t.TempDir(), "new")
	active := true
	live := func(id string) (bool, error) {
		if id != "owner-session" {
			t.Fatalf("unexpected owner %q", id)
		}
		return active, nil
	}
	if r := Check(state, "owner-session", dir, true, live); r != "" {
		t.Fatal(r)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: unused"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, filepath.Join(dir, "sub")} {
		if r := Check(state, "other-session", p, false, live); !strings.Contains(r, "owner-session") {
			t.Fatalf("foreign session allowed: %q", r)
		}
		if r := Check(state, "owner-session", p, false, live); r != "" {
			t.Fatal(r)
		}
	}
	if r := Check(state, "other-session", dir, true, live); !strings.Contains(r, "owner-session") {
		t.Fatal(r)
	}
	active = false
	if r := Check(state, "other-session", dir, false, live); r != "" {
		t.Fatal(r)
	}
	if err := os.Remove(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
	if r := Check(state, "other-session", dir, true, live); r != "" {
		t.Fatal(r)
	}
	if r := Check(state, "third-session", dir, true, func(id string) (bool, error) { return id == "other-session", nil }); !strings.Contains(r, "other-session") {
		t.Fatal(r)
	}
}

func TestWorktreeOwnerUnknownAndUnavailable(t *testing.T) {
	state, dir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := Check(state, "visitor", dir, false, nil); r != "" {
		t.Fatal(r)
	}
	fresh := filepath.Join(t.TempDir(), "fresh")
	live := func(string) (bool, error) { return false, errors.New("unavailable") }
	if r := Check(state, "owner", fresh, true, live); r != "" {
		t.Fatal(r)
	}
	if r := Check(state, "visitor", fresh, true, live); r != unavailable {
		t.Fatal(r)
	}
}

func TestWorktreeOwnerConcurrentReservations(t *testing.T) {
	state := t.TempDir()
	dir := filepath.Join(t.TempDir(), "worktree")
	results := make(chan string, 2)
	start := make(chan struct{})
	for _, session := range []string{"first", "second"} {
		go func() {
			<-start
			results <- Check(state, session, dir, true, func(string) (bool, error) { return true, nil })
		}()
	}
	close(start)
	allowed := 0
	for range 2 {
		if r := <-results; r == "" {
			allowed++
		} else if !strings.Contains(r, "owned by live session") {
			t.Fatal(r)
		}
	}
	if allowed != 1 {
		t.Fatalf("%d concurrent owners accepted", allowed)
	}
}

func TestWorktreeOwnerSymlink(t *testing.T) {
	state, root := t.TempDir(), t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	live := func(string) (bool, error) { return true, nil }
	if r := Check(state, "owner", filepath.Join(alias, "new"), true, live); r != "" {
		t.Fatal(r)
	}
	if err := os.Mkdir(filepath.Join(real, "new"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "new", ".git"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := Check(state, "visitor", filepath.Join(real, "new"), false, live); !strings.Contains(r, "owner") {
		t.Fatal(r)
	}
}
