package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnswerPreservesPlantedReplyStagingObjects(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "regular"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "spool")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(root, "public-canary")
			const canary = "fixture must survive"
			if err := os.WriteFile(victim, []byte(canary), 0o600); err != nil {
				t.Fatal(err)
			}
			planted := filepath.Join(dir, "x"+replySuffix+tmpSuffix)
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(victim, planted)
			case "hardlink":
				err = os.Link(victim, planted)
			case "regular":
				err = os.WriteFile(planted, []byte(canary), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(planted)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "x"+reqSuffix)
			if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
			answer(context.Background(), path, filepath.Join(root, "no-proc"), func(context.Context, int, Request) (Reply, error) {
				t.Fatal("malformed request reached handler")
				return Reply{}, nil
			})
			for _, name := range []string{victim, planted} {
				if b, err := os.ReadFile(name); err != nil || string(b) != canary { //nolint:gosec // public fixture files
					t.Errorf("%s changed: %q, %v", name, b, err)
				}
			}
			if after, err := os.Lstat(planted); err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Errorf("planted entry replaced: %v", err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "x"+replySuffix)) //nolint:gosec // fixture reply
			if err != nil {
				t.Fatal(err)
			}
			var reply Reply
			if err := json.Unmarshal(raw, &reply); err != nil || !strings.Contains(reply.Error, "malformed request") {
				t.Errorf("reply %s: %v", raw, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 3 {
				t.Errorf("staging file left behind: %v, %v", entries, err)
			}
		})
	}
}

func TestAnswerRemovesStagingFileAfterRenameFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x"+reqSuffix)
	if err := os.WriteFile(path, []byte(`{"op":"ping"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path) //nolint:gosec // hold the fixture request for authorization
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	called := false
	answer(context.Background(), path, "/proc", func(context.Context, int, Request) (Reply, error) {
		called = true
		// Create a destination directory after answer's initial existence check.
		if err := os.Mkdir(filepath.Join(dir, "x"+replySuffix), 0o700); err != nil {
			t.Fatal(err)
		}
		return Reply{}, nil
	})
	if !called {
		t.Fatal("handler was not called")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), tmpSuffix) {
			t.Errorf("staging file left behind: %s", entry.Name())
		}
	}
}
