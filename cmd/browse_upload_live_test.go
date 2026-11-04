//go:build browselive

package cmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestBrowseUploadLive checks the undocumented CLAUDE_STAGE_FILE_ROOT contract
// using the real browse command, installed Claude CLI and connected Chrome.
// Opt in explicitly: go test -tags browselive -run '^TestBrowseUploadLive$' -count=1 -timeout=10m ./cmd
// Requires authenticated Claude, its Chrome extension and the normal browse
// configuration/hooks. Missing prerequisites fail rather than silently skip.
func TestBrowseUploadLive(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	dir := t.TempDir()
	binary := filepath.Join(dir, "beekeeper")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	// The random bytes are never in the prompt or page: a successful report or
	// an invented upload cannot satisfy the check.
	payload := make([]byte, 128)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "upload-proof.bin")
	if err := os.WriteFile(file, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	received := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, `<!doctype html><title>Browse upload proof</title>
<form id="upload"><label>Proof file <input type="file" name="proof"></label><button>Upload proof</button></form><p id="result"></p>
<script>document.querySelector('form').onsubmit=async e=>{e.preventDefault();const r=await fetch('/upload',{method:'POST',body:new FormData(e.target)});document.querySelector('#result').textContent=await r.text();};</script>`)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/upload" {
			http.NotFound(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, "invalid upload", http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()
		f, header, err := r.FormFile("proof")
		if err != nil {
			http.Error(w, "missing file", http.StatusBadRequest)
			return
		}
		defer f.Close()
		data, err := io.ReadAll(f)
		if err != nil || header.Filename != "upload-proof.bin" || !bytes.Equal(data, payload) {
			http.Error(w, "file mismatch", http.StatusBadRequest)
			return
		}
		select {
		case received <- struct{}{}:
		default:
		}
		_, _ = io.WriteString(w, "Upload verified")
	}))
	defer server.Close()
	steps := fmt.Sprintf("Open %s in a new tab. Upload the shared upload-proof.bin through the visible file input in form #upload using file_upload, following the upload-target check. Click Upload proof and wait for Upload verified. Do not synthesize file contents or send requests through javascript_tool. Report the result and close the tab.", server.URL)
	browse := exec.CommandContext(ctx, binary, "browse", "--dir", dir, "--timeout", "5m", "--file", file, steps)
	out, err := browse.CombinedOutput()
	if err != nil {
		t.Fatalf("browse --file failed: %v\n%s", err, out)
	}
	select {
	case <-received:
	default:
		t.Fatalf("browse did not upload the expected bytes; check undocumented CLAUDE_STAGE_FILE_ROOT compatibility\n%s", out)
	}
}
