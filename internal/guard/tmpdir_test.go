package guard

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tmpfsAndDisk are a tmpfs directory and one on disk, or a skip.
func tmpfsAndDisk(t *testing.T) (tmpfs, disk string) {
	t.Helper()
	disk, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if !onTmpfs("/dev/shm") || onTmpfs(disk) {
		t.Skip("needs /dev/shm on tmpfs and the package directory on disk")
	}
	return "/dev/shm", disk
}

func TestRunPointsTheCommandsTempFilesOffTmpfs(t *testing.T) {
	shm, _ := tmpfsAndDisk(t)
	f := &fakeCapper{}
	saved := plat.Capper
	plat.Capper = f
	t.Cleanup(func() { plat.Capper = saved })
	slots, base, out := t.TempDir(), filepath.Join(t.TempDir(), "tmp"), filepath.Join(t.TempDir(), "env")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "fresh")
	t.Setenv("GOTMPDIR", "")
	t.Setenv("TMPDIR", shm)
	o := Options{Max: "1M", Swap: "0", SlotDir: slots, Slots: 1, Stderr: io.Discard, TmpDir: base}
	script := `test -d "$GOTMPDIR" && printf '%s\n%s\n' "$GOTMPDIR" "$TMPDIR" > "$1"`
	if rc := Run(o, []string{"sh", "-c", script, "sh", out}); rc != 0 {
		t.Fatalf("exit %d, want 0", rc)
	}
	raw, err := os.ReadFile(out) //nolint:gosec // the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(raw))
	if len(got) != 2 || got[0] != got[1] || filepath.Dir(got[0]) != base {
		t.Errorf("GOTMPDIR and TMPDIR %q: want one directory in %s", got, base)
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Errorf("the run's temporary directory outlived it: %v", entries)
	}
}

func TestRunTmpKeepsTheCallersDirectoryOffTmpfs(t *testing.T) {
	shm, disk := tmpfsAndDisk(t)
	base := filepath.Join(t.TempDir(), "tmp")
	t.Setenv("GOTMPDIR", disk)
	t.Setenv("TMPDIR", disk)
	env, cleanup := runTmp(base, t.Logf)
	cleanup()
	if env != nil {
		t.Errorf("caller's directories on disk: env %q, want none", env)
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Errorf("caller's directories on disk: %s made (%v), want nothing", base, err)
	}

	t.Setenv("TMPDIR", shm)
	env, cleanup = runTmp(base, t.Logf)
	defer cleanup()
	if len(env) != 1 || !strings.HasPrefix(env[0], "TMPDIR="+base+string(filepath.Separator)) {
		t.Errorf("caller's TMPDIR on tmpfs: env %q, want only TMPDIR in %s", env, base)
	}
}

func TestPruneTmpRemovesOnlyLeftovers(t *testing.T) {
	base := t.TempDir()
	old, fresh := filepath.Join(base, "old"), filepath.Join(base, "fresh")
	for _, d := range []string{old, fresh} {
		if err := os.MkdirAll(filepath.Join(d, "go-build1"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	then := time.Now().Add(-2 * staleTmp)
	if err := os.Chtimes(old, then, then); err != nil {
		t.Fatal(err)
	}
	pruneTmp(base, time.Now().Add(-staleTmp))
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("a leftover older than %s stayed: %v", staleTmp, err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a running run's directory went: %v", err)
	}
}
