package board

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
)

func TestMetadataCache(t *testing.T) {
	f := &fakeGH{t: t}
	c := &Client{GH: f.run, CacheDir: t.TempDir(), Board: config.Board{Owner: "o", Project: 7}}
	for range 2 {
		if _, err := c.meta(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.queries) != 1 {
		t.Fatalf("metadata read %d times", len(f.queries))
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(c.metaPath(), old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := c.meta(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.queries) != 2 {
		t.Fatal("expired metadata reused")
	}
	c.Board.Project = 8
	if c.cachedMeta() != nil {
		t.Fatal("cache reused for another project")
	}
}
