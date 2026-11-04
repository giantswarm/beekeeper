package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReadRejectsARequestReplacedByFIFO(t *testing.T) {
	for _, writer := range []bool{false, true} {
		name := "without writer"
		if writer {
			name = "idle writer"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "x"+reqSuffix)
			if err := os.WriteFile(path, []byte(`{"op":"ping"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || !entries[0].Type().IsRegular() {
				t.Fatalf("listed request is not regular: %v, %v", entries, err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			var idle *os.File
			if writer {
				idle, err = os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0) //nolint:gosec // fixture FIFO
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = idle.Close() }()
			}
			done := make(chan error, 1)
			go func() {
				_, _, err := read(path)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "not a regular file") {
					t.Errorf("FIFO refusal = %v", err)
				}
			case <-time.After(time.Second):
				// Unblock an unfixed implementation before failing the test.
				// RDWR releases a blocked open; closing it releases a read.
				if idle != nil {
					_ = idle.Close()
				} else {
					rescue, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0) //nolint:gosec // fixture FIFO
					if err != nil {
						t.Fatal(err)
					}
					_ = rescue.Close()
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("fixture reader did not stop after rescue")
				}
				t.Fatal("request read blocked on substituted FIFO")
			}
		})
	}
}

func TestReadRejectsDirectoriesAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link"+reqSuffix)
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, link} {
		if _, _, err := read(path); err == nil {
			t.Errorf("non-regular request accepted: %s", path)
		}
	}
}
