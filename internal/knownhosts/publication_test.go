package knownhosts

import (
	"os"
	"path/filepath"
	"testing"
)

func assertContents(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("file contents = %q, want %q", got, want)
	}
}

func assertNoPublicationFiles(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".known_hosts-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("publication files remain: %v", matches)
	}
}

func TestParseablePreservesExistingWhenAllParse(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "destination")
	old := hostLine(t, "*") + "\n"
	write(t, dst, old[:len(old)-1])
	write(t, src, hostLine(t, "*"))
	dropped, err := Parseable(dst, src)
	if err != nil || dropped != 0 {
		t.Fatalf("dropped %d, err %v", dropped, err)
	}
	assertContents(t, dst, old)
	assertNoPublicationFiles(t, dir)
}

func TestParseableFailurePreservesExisting(t *testing.T) {
	for _, failure := range []string{"source read", "destination setup", "publication rename"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "destination")
			old := hostLine(t, "*") + "\n"
			write(t, src, hostLine(t, "*"), unknownLine("*"))
			preserved := dst
			srcs := []string{src}
			switch failure {
			case "source read":
				// Reading a directory fails even when the tests run with elevated permissions.
				srcs = append(srcs, dir)
			case "destination setup":
				// A regular file cannot serve as the destination's parent directory.
				dst = filepath.Join(dst, "child")
			case "publication rename":
				// A nonempty directory cannot be replaced by the publication file.
				if err := os.Mkdir(dst, 0o700); err != nil {
					t.Fatal(err)
				}
				preserved = filepath.Join(dst, "previous")
			}
			write(t, preserved, old[:len(old)-1])
			if _, err := Parseable(dst, srcs...); err == nil {
				t.Fatal("expected failure")
			}
			assertContents(t, preserved, old)
			assertNoPublicationFiles(t, dir)
		})
	}
}
