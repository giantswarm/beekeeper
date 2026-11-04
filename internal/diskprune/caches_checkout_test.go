package diskprune

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheFilesKeepsCheckoutCreatedDuringScan(t *testing.T) {
	for _, location := range []string{".", "repo"} {
		for _, gitfile := range []bool{false, true} {
			name := "directory"
			if gitfile {
				name = "gitfile"
			}
			t.Run(filepath.Join(location, name), func(t *testing.T) {
				root := t.TempDir()
				source := cacheFile(t, root, filepath.Join(location, "source"), 1000, 10*day)
				other := cacheFile(t, root, "other", 1000, 10*day)
				p := Policy{Root: root, Files: true, OlderThan: 7 * day, Recheck: func() ([]string, error) {
					marker := filepath.Join(root, location, checkoutMetadata)
					if gitfile {
						return nil, os.WriteFile(marker, []byte("gitdir: synthetic"), 0600)
					}
					return nil, os.Mkdir(marker, 0700)
				}}
				r := Cache(p, time.Now(), false, nil, nil, false)
				if r.Freed != 0 || r.Removed != 0 || r.Kept[whyCheckout] != 1 || !exists(source) || !exists(other) {
					t.Fatalf("new checkout lost files: %+v, source=%v other=%v", r, exists(source), exists(other))
				}
			})
		}
	}
}
