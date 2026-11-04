package free

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionDirsStayInsideScratch(t *testing.T) {
	for _, link := range []string{"project", "session"} {
		t.Run(link, func(t *testing.T) {
			r, out := newRun(t, Options{Apply: true, Only: []string{SessionDirs}})
			outside := t.TempDir()
			rel := filepath.Join(deadSID, "scratchpad", "keep")
			if link == "session" {
				rel = filepath.Join("scratchpad", "keep")
			}
			fixture(t, outside, rel, 1, 5*time.Hour)
			path := filepath.Join(r.Scratch, "-outside")
			if link == "session" {
				path = filepath.Join(path, deadSID)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if got := r.Free(); len(got) != 0 {
				t.Errorf("counted scratch outside its root: %+v\n%s", got, out)
			}
			if _, err := os.Stat(filepath.Join(outside, rel)); err != nil {
				t.Errorf("cleanup changed the symlink target: %v\n%s", err, out)
			}
		})
	}
}
