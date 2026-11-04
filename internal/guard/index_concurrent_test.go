package guard

import (
	"sync"
	"testing"
	"time"
)

func TestIndexConcurrentFirstUse(t *testing.T) {
	dir := t.TempDir()
	const callers = 32
	type result struct {
		index *Index
		err   error
	}
	results := make(chan result, callers)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(callers)
	for range callers {
		go func() {
			ready.Done()
			<-start
			ix, err := OpenIndex(dir)
			results <- result{ix, err}
		}()
	}
	ready.Wait()
	close(start)
	var opened []*Index
	for range callers {
		r := <-results
		if r.err != nil {
			t.Errorf("concurrent first use: %v", r.err)
			continue
		}
		opened = append(opened, r.index)
	}
	persisted, err := LoadIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, ix := range opened {
		if ix.Fingerprint(planted) != persisted.Fingerprint(planted) {
			t.Fatal("a first-use caller retained a different key from the persisted index")
		}
	}
	// A successful caller's fingerprints must still redact after a reload.
	if len(opened) == 0 {
		t.Fatal("no first-use caller succeeded")
	}
	opened[0].Add(plantedRef, planted)
	if err := opened[0].Save(time.Now()); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := reloaded.Redact(planted); len(found) != 1 || found[0].Ref != plantedRef {
		t.Fatal("the first-use index lost its value after reload")
	}
}
