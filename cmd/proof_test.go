package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// proof upload needs --repo and an image, and writes nothing without them.
func TestProofUploadRefuses(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	called := false
	old := proofGH
	proofGH = func(context.Context, []byte, ...string) ([]byte, error) { called = true; return nil, nil }
	t.Cleanup(func() { proofGH = old })

	text := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(text, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := execute(t, "proof", "upload", text); err == nil || !strings.Contains(err.Error(), "--repo") {
		t.Errorf("without --repo: %v", err)
	}
	if _, err := execute(t, "proof", "upload", text, "--repo", "o/r"); err == nil || !strings.Contains(err.Error(), "not a PNG, JPEG or GIF") {
		t.Errorf("a text file: %v", err)
	}
	if called {
		t.Error("gh was called for a refused upload")
	}
}
