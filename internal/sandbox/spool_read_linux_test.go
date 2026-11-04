package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReadRefusesSubstitutedFIFO(t *testing.T) {
	for _, idleWriter := range []bool{false, true} {
		name := "without_writer"
		if idleWriter {
			name = "idle_writer"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "request"+reqSuffix)
			if err := os.WriteFile(path, []byte(`{"op":"ping"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || !entries[0].Type().IsRegular() {
				t.Fatal("request was not listed as regular")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			// O_RDWR supplies an idle synthetic writer without waiting for a reader.
			var writer int
			if idleWriter {
				writer, err = syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK, 0)
				if err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() { _, _, err := read(filepath.Join(dir, entries[0].Name())); done <- err }()
			select {
			case err := <-done:
				if idleWriter {
					_ = syscall.Close(writer)
				}
				if err == nil || !strings.Contains(err.Error(), "not a regular file") {
					t.Fatalf("FIFO refusal: %v", err)
				}
			case <-time.After(time.Second):
				// Rescue a regressed blocking open/read, with a bounded cleanup wait.
				if !idleWriter {
					writer, err = syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
					if err != nil {
						t.Errorf("rescue writer: %v", err)
					}
				}
				if err == nil {
					_ = syscall.Close(writer)
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("read did not end after rescue")
				}
				t.Fatal("FIFO read blocked")
			}
		})
	}
}

func TestReadRefusesDirectoryAndSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "regular")
	if err := os.WriteFile(target, []byte("request"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, link} {
		if _, _, err := read(path); err == nil {
			t.Errorf("read accepted %s", filepath.Base(path))
		}
	}
}

func TestReadRegularRequestAndLimit(t *testing.T) {
	for _, size := range []int{0, 16, maxRequest, maxRequest + 1, maxRequest + 100} {
		path := filepath.Join(t.TempDir(), "request"+reqSuffix)
		content := strings.Repeat("x", size)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		raw, st, err := read(path)
		want := min(size, maxRequest+1)
		if err != nil {
			t.Fatal(err)
		}
		if st == nil || !st.Mode().IsRegular() || string(raw) != content[:want] {
			t.Errorf("size %d: got %d bytes, stat %v", size, len(raw), st)
		}
	}
}
