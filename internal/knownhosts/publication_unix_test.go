//go:build unix

package knownhosts

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh/knownhosts"
)

// An already open reader must retain the old snapshot across replacement.
// os.Rename only guarantees atomic replacement on Unix.
func TestParseableConcurrentPublication(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "destination")
	old := hostLine(t, "old.example") + "\n"
	if err := os.WriteFile(dst, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(dst) //nolint:gosec // synthetic temp fixture
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	var sources, snapshots []string
	for _, name := range []string{"first.example", "second.example"} {
		src := filepath.Join(dir, name)
		// Multiple lines distinguish a complete snapshot from an interleaved
		// or truncated write without relying on a timed reader loop.
		snapshot := strings.Repeat(hostLine(t, name)+"\n", 128)
		if err := os.WriteFile(src, []byte(snapshot+unknownLine("pq.example")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		sources = append(sources, src)
		snapshots = append(snapshots, snapshot)
	}
	start := make(chan struct{})
	results := make(chan error, len(sources))
	for _, src := range sources {
		go func() {
			<-start
			dropped, err := Parseable(dst, src)
			if err == nil && dropped != 1 {
				err = fmt.Errorf("dropped %d lines, want 1", dropped)
			}
			results <- err
		}()
	}
	close(start)
	for range sources {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	readOld, err := io.ReadAll(reader)
	if err != nil || string(readOld) != old {
		t.Fatalf("open reader lost its original complete snapshot: error=%v", err)
	}
	current, err := os.ReadFile(dst) //nolint:gosec // synthetic temp fixture
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != snapshots[0] && string(current) != snapshots[1] {
		t.Fatal("destination is not one complete replacement snapshot")
	}
	if _, err := knownhosts.New(dst); err != nil {
		t.Fatalf("published trust file does not parse: %v", err)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, ".known_hosts-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("publication files left behind: %v, error=%v", leftovers, err)
	}
}
