package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
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

// browse reports the uploaded Markdown, and fails visibly when the App
// cannot write, rather than presenting a local path as hosted proof.
func TestUploadBrowseProofs(t *testing.T) {
	var img bytes.Buffer
	if err := png.Encode(&img, image.NewGray(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(file, img.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	old := proofGH
	t.Cleanup(func() { proofGH = old })
	calls := 0
	proofGH = func(_ context.Context, input []byte, args ...string) ([]byte, error) {
		calls++
		if strings.Join(args, " ") == "api repos/o/r --jq .visibility" {
			return []byte("private"), nil
		}
		if strings.Contains(strings.Join(args, " "), "/git/ref/heads/") {
			return nil, errors.New("HTTP 404")
		}
		var body map[string]any
		if err := json.Unmarshal(input, &body); err != nil {
			t.Fatal(err)
		}
		return []byte(`{"sha":"commit"}`), nil
	}
	a := &app{}
	if lines, err := a.uploadBrowseProofs(context.Background(), []string{file}, ""); err != nil || len(lines) != 0 || calls != 0 {
		t.Fatalf("unrequested upload: %v %v calls=%d", lines, err, calls)
	}
	lines, err := a.uploadBrowseProofs(context.Background(), []string{file}, "o/r")
	if err != nil || len(lines) != 1 || !strings.HasPrefix(lines[0], "proof: ![shot](../../../o/r/blob/commit/") || !strings.HasSuffix(lines[0], "?raw=true)") {
		t.Fatalf("hosted report: %v %v", lines, err)
	}
	proofGH = func(context.Context, []byte, ...string) ([]byte, error) { return nil, errors.New("HTTP 403") }
	lines, err = a.uploadBrowseProofs(context.Background(), []string{file}, "o/r")
	if err == nil || len(lines) != 1 || !strings.Contains(lines[0], "proof upload failed: HTTP 403") {
		t.Fatalf("failed report: %v %v", lines, err)
	}
}

// The command line enters through browseWithProof: the permanent deletion
// preflight holds there, before any upload or turn.
func TestBrowseWithProofKeepsDeletionPreflight(t *testing.T) {
	a := &app{}
	err := a.browseWithProof(context.Background(), "Delete my comment.", "", "", 0, "", nil, nil, "o/r")
	if err == nil || !strings.Contains(err.Error(), permanentDeletionRule) {
		t.Fatalf("browseWithProof = %v, want permanent deletion rule", err)
	}
}
