//go:build unix

package knownhosts

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh/knownhosts"
)

// os.Rename's atomic replacement guarantee is specific to Unix platforms.
func TestParseableConcurrentAtomicReplacement(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "destination")
	old := strings.Repeat(hostLine(t, "*")+"\n", 128)
	write(t, dst, strings.TrimSuffix(old, "\n"))
	reader, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	const publishers = 8
	sources := make([]string, publishers)
	snapshots := make(map[string]bool, publishers)
	for i := range sources {
		// Distinct synthetic keys make each complete replacement distinguishable.
		snapshot := strings.Repeat(hostLine(t, "*")+"\n", 128)
		snapshots[snapshot] = true
		sources[i] = filepath.Join(t.TempDir(), "source")
		write(t, sources[i], strings.TrimSuffix(snapshot, "\n"), unknownLine("*"))
	}
	start := make(chan struct{})
	results := make(chan error, publishers)
	var ready sync.WaitGroup
	ready.Add(publishers)
	for _, src := range sources {
		go func() {
			ready.Done()
			<-start
			dropped, err := Parseable(dst, src)
			if err == nil && dropped != 1 {
				err = fmt.Errorf("dropped %d lines, want 1", dropped)
			}
			results <- err
		}()
	}
	ready.Wait()
	close(start)
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for range publishers {
		select {
		case err := <-results:
			if err != nil {
				t.Error(err)
			}
		case <-deadline.C:
			t.Fatal("concurrent publication did not finish")
		}
	}
	previous, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(previous) != old {
		t.Fatal("open reader lost the complete old snapshot")
	}
	current, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshots[string(current)] {
		t.Fatal("destination does not contain one complete new snapshot")
	}
	if _, err := knownhosts.New(dst); err != nil {
		t.Fatalf("replacement does not parse: %v", err)
	}
	assertNoPublicationFiles(t, dir)
}
