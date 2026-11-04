package proc

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBootTimeAfterLongInterruptLine(t *testing.T) {
	root := t.TempDir()
	// /proc/stat has one column per interrupt, all on the same line.
	// Large interrupt tables exceed bufio.Scanner's default token limit.
	raw := "cpu 1 2 3 4\nintr 1" + strings.Repeat(" 0", 40000) + "\nbtime 1700000000\n"
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := bootTime(root)
	if want := time.Unix(1700000000, 0); err != nil || !got.Equal(want) {
		t.Fatalf("bootTime = %s, %v; want %s", got, err, want)
	}
}

func TestBootTimeEndOfFile(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		wantErr   bool
	}{
		{"no final newline", "btime 1700000000", false},
		{"missing btime", "cpu 1 2 3 4\n", true},
		{"invalid btime", "btime invalid\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "stat"), []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := bootTime(root)
			if (err != nil) != tc.wantErr {
				t.Fatalf("bootTime error = %v, want error = %v", err, tc.wantErr)
			}
			if !tc.wantErr && !got.Equal(time.Unix(1700000000, 0)) {
				t.Fatalf("bootTime = %s", got)
			}
		})
	}
}

func TestBootTimeReportsReadError(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "stat"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := bootTime(root)
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("lost underlying file error: %v", err)
	}
}
