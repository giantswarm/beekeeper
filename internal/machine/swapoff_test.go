package machine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSwapoffIn(t *testing.T) {
	dir := t.TempDir()
	write := func(pid, comm string) {
		if err := os.MkdirAll(filepath.Join(dir, pid), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, pid, "comm"), []byte(comm+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("1", "systemd")
	write("42", "swapon")
	if swapoffIn(dir) {
		t.Fatal("no swapoff runs, but swapoffIn says one does")
	}
	write("4711", "swapoff")
	if !swapoffIn(dir) {
		t.Fatal("swapoff runs, but swapoffIn misses it")
	}
}
