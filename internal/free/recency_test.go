package free

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFreeKeepsRecentlyChangedDirectories(t *testing.T) {
	for section, dir := range map[string]string{
		SessionDirs: sessionDir(deadSID),
		GoBuild:     "go-build-recent",
		TmpDirs:     "tmp.recent",
	} {
		t.Run(section, func(t *testing.T) {
			for _, changed := range []string{".", "scratchpad"} {
				t.Run(changed, func(t *testing.T) {
					r, out := newRun(t, Options{Apply: true, Only: []string{section}})
					fixture(t, r.TmpDir, filepath.Join(dir, "scratchpad", "keep"), 2048, 5*time.Hour)
					candidate := filepath.Join(r.TmpDir, dir)
					// Model copying an old file with preserved times into a new
					// directory, or changing entries in an existing subdirectory.
					if err := os.Chtimes(filepath.Join(candidate, changed), now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
						t.Fatal(err)
					}
					if got := r.Free(); len(got) != 0 {
						t.Errorf("freed a recently changed directory: %+v\n%s", got, out)
					}
					if _, err := os.Stat(filepath.Join(candidate, "scratchpad", "keep")); err != nil {
						t.Errorf("recent content removed: %v\n%s", err, out)
					}
				})
			}
		})
	}
}
