package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBinaryReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "beekeeper")
	write := func(p string, mode os.FileMode) {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(path, 0o755)
	self, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	b := &binary{path: path, self: self}
	if b.replaced() {
		t.Error("the running file reads as replaced")
	}
	write(path+".new", 0o644)
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	if b.replaced() {
		t.Error("a file that is not executable reads as a replacement")
	}
	write(path+".new", 0o755)
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	if !b.replaced() {
		t.Error("a new binary renamed over the path is not noticed")
	}
	var none *binary
	if none.replaced() {
		t.Error("an unknown binary reads as replaced")
	}
	if runningBinary() == nil {
		t.Error("the test binary is not found")
	}
}
