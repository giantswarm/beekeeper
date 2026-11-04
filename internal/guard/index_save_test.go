package guard

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// Separate snapshots may replace one another, but each Save must publish one
// complete snapshot without colliding with another writer's temporary file.
func TestIndexConcurrentSavesPublishCompleteSnapshots(t *testing.T) {
	const writers = 32
	const entries = 64
	dir := t.TempDir()
	indexes := make([]*Index, writers)
	for i := range indexes {
		ix, err := OpenIndex(dir)
		if err != nil {
			t.Fatal(err)
		}
		for j := range entries {
			ix.Add(fmt.Sprintf("fixture/%02d/%02d", i, j), fmt.Sprintf("synthetic-index-save-%02d-%02d", i, j))
		}
		indexes[i] = ix
	}
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for _, ix := range indexes {
		wg.Go(func() {
			<-start
			errs <- ix.Save(time.Unix(1, 0))
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Save: %v", err)
		}
	}
	loaded, err := LoadIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	refs := loaded.Refs()
	if len(refs) != entries {
		t.Fatalf("published %d references, want one complete snapshot of %d", len(refs), entries)
	}
	writer := refs[0][:len("fixture/00/")]
	for _, ref := range refs {
		if ref[:len(writer)] != writer {
			t.Fatalf("published references from multiple snapshots: %v", refs)
		}
	}
}
