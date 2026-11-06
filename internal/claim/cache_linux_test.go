package claim_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"syscall"
	"testing"

	"github.com/giantswarm/beekeeper/internal/claim"
	"github.com/giantswarm/beekeeper/internal/claim/claimtest"
)

func TestClaimCacheClosesFailedWrites(t *testing.T) {
	const child = "BEEKEEPER_TEST_FAILED_CACHE_WRITES"
	if os.Getenv(child) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestClaimCacheClosesFailedWrites$", "-test.count=1") //nolint:gosec // a path of this test
		cmd.Env = append(os.Environ(), child+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated cache test: %v\n%s", err, out)
		}
		return
	}

	// These process-wide settings belong only to this isolated test process.
	runtime.GC()
	previousGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(previousGC)
	var previousLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &previousLimit); err != nil {
		t.Fatal(err)
	}
	limit := previousLimit
	limit.Cur = 1
	for _, existing := range []bool{false, true} {
		name := "missing cache"
		if existing {
			name = "existing cache"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cache := filepath.Join(dir, "cache.json")
			original := []byte("{}")
			if existing {
				if err := os.WriteFile(cache, original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &previousLimit); err != nil {
					t.Error(err)
				}
			}()
			probe, err := os.CreateTemp(dir, "probe-*")
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := probe.Write([]byte("cache"))
			closeErr := probe.Close()
			if !errors.Is(writeErr, syscall.EFBIG) || closeErr != nil {
				t.Fatalf("write limit probe: write %v, close %v", writeErr, closeErr)
			}
			if err := os.Remove(probe.Name()); err != nil {
				t.Fatal(err)
			}

			gh := claimtest.New()
			marker := claim.Marker{Desk: deskTwo, Session: "s", At: t0}
			id := gh.Add(issue, marker.Body())
			cl := &claim.Client{GH: gh.GH, Cache: cache}
			countFDs := func() int {
				t.Helper()
				entries, err := os.ReadDir("/proc/self/fd")
				if err != nil {
					t.Fatal(err)
				}
				return len(entries)
			}
			before := countFDs()
			for range 3 {
				cs, err := cl.Read(context.Background(), issue)
				if err != nil || len(cs) != 1 || cs[0].ID != id || cs[0].Marker != marker {
					t.Fatalf("Read = %+v, %v", cs, err)
				}
			}
			if delta := countFDs() - before; delta != 0 {
				t.Errorf("failed cache writes left %d file descriptors open", delta)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if existing {
				raw, err := os.ReadFile(cache) //nolint:gosec // a path of this test
				if err != nil || !bytes.Equal(raw, original) {
					t.Fatalf("previous cache changed: %q, %v", raw, err)
				}
				if len(entries) != 1 || entries[0].Name() != "cache.json" {
					t.Fatalf("unexpected cache directory entries: %v", entries)
				}
			} else if len(entries) != 0 {
				t.Fatalf("failed writes left cache directory entries: %v", entries)
			}
		})
	}
}
