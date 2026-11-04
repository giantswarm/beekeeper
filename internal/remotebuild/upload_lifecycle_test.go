//nolint:goconst // Repeated fixture and protocol literals make the failure scenarios readable.
package remotebuild

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRemoteCancelledUploadPreservesNextRun(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux builder shell contract")
	}
	dir, bin := t.TempDir(), t.TempDir()
	// The first tar waits for input after reporting readiness. The second
	// succeeds immediately, so the old uploader exits after the next owns busy.
	fakeTar := "#!/bin/bash\nif [ \"${FIXTURE_UPLOAD:-}\" = old ]; then printf ready; read -r _; exit 19; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "tar"), []byte(fakeTar), 0o700); err != nil { //nolint:gosec // executable fixture
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	script := func(s string) string { return strings.ReplaceAll(s, "/work", dir) }
	old := exec.CommandContext(ctx, shellBash, "-c", script(upload), "--", "old") //nolint:gosec // production script in an owned fixture
	old.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FIXTURE_UPLOAD=old")
	old.WaitDelay = time.Second
	release, err := old.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release.Close() }()
	ready, err := old.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Process.Kill() }()
	if _, err := io.ReadFull(ready, make([]byte, len("ready"))); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct{ script, id string }{{interrupt, "old"}, {upload, "new"}} {
		cmd := exec.CommandContext(ctx, shellBash, "-c", script(step.script), "--", step.id) //nolint:gosec // production script in an owned fixture
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FIXTURE_UPLOAD=new")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", step.id, err, out)
		}
	}
	marker := filepath.Join(dir, "runs/new/incoming/new-source")
	if err := os.WriteFile(marker, []byte("new source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(release, "finish\n"); err != nil {
		t.Fatal(err)
	}
	if err := old.Wait(); err == nil {
		t.Fatal("old tar must fail")
	}
	id, err := os.ReadFile(filepath.Join(dir, "busy/id")) //nolint:gosec // owned fixture
	if err != nil || string(id) != "new" {
		t.Errorf("cancelled upload erased the next reservation: id=%q error=%v", id, err)
	}
	if source, err := os.ReadFile(marker); err != nil || string(source) != "new source" { //nolint:gosec // owned fixture
		t.Errorf("cancelled upload erased the next source: source=%q error=%v", source, err)
	}
}

func TestRemoteDelayedWrapperCannotReplaceNextSource(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux builder shell contract")
	}
	dir := t.TempDir()
	for _, path := range []string{"busy", "src", "incoming", "runs/old/incoming", "runs/new/incoming"} {
		if err := os.MkdirAll(filepath.Join(dir, path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, data := range map[string]string{"busy/id": "new", "src/source": "original source", "incoming/source": "next source", "runs/new/incoming/source": "next source", "runs/old/log": ""} {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	marker := filepath.Join(dir, "stale-command-ran")
	cmd := exec.CommandContext(ctx, shellBash, "-c", strings.ReplaceAll(wrapper, "/work", dir), "--", "old", ".", "touch "+quote(marker)) //nolint:gosec // production script in an owned fixture
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("cancelled wrapper ran: %s", out)
	}
	if source, err := os.ReadFile(filepath.Join(dir, "src/source")); err != nil || string(source) != "original source" { //nolint:gosec // owned fixture
		t.Errorf("stale wrapper replaced source: source=%q error=%v", source, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("stale command executed: %v", err)
	}
}
