package knownhosts

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseablePreservesDestinationOnFailure(t *testing.T) {
	for _, failure := range []string{"later source read", "destination directory creation", "publication rename"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "destination")
			write(t, src, hostLine(t, "new.example"), unknownLine("pq.example"))
			old := hostLine(t, "old.example") + "\n"
			preserved := dst
			srcs := []string{src}
			switch failure {
			case "later source read":
				unreadable := filepath.Join(dir, "source-directory")
				if err := os.Mkdir(unreadable, 0o700); err != nil {
					t.Fatal(err)
				}
				srcs = append(srcs, unreadable)
			case "destination directory creation":
				dst = filepath.Join(preserved, "child")
			case "publication rename":
				if err := os.Mkdir(dst, 0o700); err != nil {
					t.Fatal(err)
				}
				preserved = filepath.Join(dst, "existing")
			}
			if err := os.WriteFile(preserved, []byte(old), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Parseable(dst, srcs...); err == nil {
				t.Fatal("expected the filesystem failure to be returned")
			}
			got, err := os.ReadFile(preserved) //nolint:gosec // synthetic temp fixture
			if err != nil || string(got) != old {
				t.Fatalf("existing content changed: error=%v", err)
			}
			leftovers, err := filepath.Glob(filepath.Join(dir, ".known_hosts-*"))
			if err != nil || len(leftovers) != 0 {
				t.Fatalf("publication files left behind: %v, error=%v", leftovers, err)
			}
		})
	}
}

func TestParseableLeavesExistingDestinationWhenUnfiltered(t *testing.T) {
	for _, source := range []string{"valid", "empty", "missing"} {
		t.Run(source, func(t *testing.T) {
			dir := t.TempDir()
			src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "destination")
			switch source {
			case "valid":
				write(t, src, hostLine(t, "current.example"))
			case "empty":
				if err := os.WriteFile(src, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			old := hostLine(t, "old.example") + "\n"
			if err := os.WriteFile(dst, []byte(old), 0o600); err != nil {
				t.Fatal(err)
			}
			if dropped, err := Parseable(dst, src); err != nil || dropped != 0 {
				t.Fatalf("dropped %d, error %v; want 0, nil", dropped, err)
			}
			got, err := os.ReadFile(dst) //nolint:gosec // synthetic temp fixture
			if err != nil || string(got) != old {
				t.Fatalf("existing destination changed: error=%v", err)
			}
		})
	}
}
