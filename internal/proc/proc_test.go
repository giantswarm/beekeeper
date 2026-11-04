package proc

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestParseStat(t *testing.T) {
	raw := []byte("4242 (tmux: server) (x)) S 1 4242 4242 0 -1 4194560 1 0 0 0 300 45 0 0 20 0 1 0 123456 1 2560 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0")
	st, err := parseStat(raw)
	want := stat{comm: "tmux: server) (x)", ppid: 1, cpu: 345, start: 123456, rssPages: 2560}
	if err != nil || st != want {
		t.Fatalf("parseStat = %+v, %v; want %+v", st, err, want)
	}
	if _, err := parseStat([]byte("1 (x) S 0 1 1")); err == nil {
		t.Fatal("a short stat parsed")
	}
}

func TestReadAtLongUptimeAndCPUTime(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "42")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// These counters fit in a duration, but multiplying by nanoseconds
	// before dividing by USER_HZ overflows. CPU time sums across threads,
	// so a busy multicore process can reach this well before years pass.
	for path, data := range map[string]string{
		filepath.Join(root, "stat"):   "btime 1700000000\n",
		filepath.Join(dir, "stat"):    "42 (worker) S 1 0 0 0 0 0 0 0 0 0 6000000000 4000000001 0 0 0 0 1 0 10000000001 0 1\n",
		filepath.Join(dir, "cmdline"): "worker\x00",
	} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	table, err := ReadAt(root)
	if err != nil {
		t.Fatal(err)
	}
	p := table.Process(42)
	if p == nil {
		t.Fatal("process missing from fixture table")
	}
	want := 100000000*time.Second + 10*time.Millisecond
	if p.CPU != want {
		t.Errorf("CPU = %s, want %s", p.CPU, want)
	}
	if start := time.Unix(1700000000, 0).Add(want); !p.Start.Equal(start) {
		t.Errorf("Start = %s, want %s", p.Start, start)
	}
	if p.StartTicks != 10000000001 {
		t.Errorf("StartTicks = %d", p.StartTicks)
	}
}

func TestReadFindsItself(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	tab, err := Read()
	if err != nil {
		t.Fatal(err)
	}
	me := tab.ByPID[os.Getpid()]
	if me == nil {
		t.Fatal("own process missing")
	}
	anc := tab.Ancestors(me.PID)
	if len(anc) == 0 || anc[0].PID != os.Getppid() {
		t.Errorf("ancestors = %v", anc)
	}
}

func TestWriteBytesAt(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "42"), 0o750); err != nil {
		t.Fatal(err)
	}
	io := "rchar: 1\nwchar: 2\nsyscr: 3\nsyscw: 4\nread_bytes: 5\nwrite_bytes: 4096\ncancelled_write_bytes: 0\n"
	if err := os.WriteFile(filepath.Join(root, "42", "io"), []byte(io), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := writeBytesAt(root, 42); err != nil || got != 4096 {
		t.Errorf("writeBytesAt = %d, %v; want 4096", got, err)
	}
	if _, err := writeBytesAt(root, 43); err == nil {
		t.Error("a missing process read without an error")
	}
}

// Held names the working directory, the executable and the open files by
// their paths, a deleted file by the one it had, and skips what links to no
// path.
func TestHeldNamesCwdExeAndOpenFiles(t *testing.T) {
	root := t.TempDir()
	fd := filepath.Join(root, "42", "fd")
	if err := os.MkdirAll(fd, 0o750); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		filepath.Join(root, "42", "cwd"): "/work",
		filepath.Join(root, "42", "exe"): "/tmp/go-build1/b001/x.test",
		filepath.Join(fd, "0"):           "/dev/null",
		filepath.Join(fd, "3"):           "socket:[1234]",
		filepath.Join(fd, "4"):           "/tmp/claude-1000/p/s/scratchpad/out.log (deleted)",
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	got := held(root, 42)
	slices.Sort(got)
	want := []string{"/dev/null", "/tmp/claude-1000/p/s/scratchpad/out.log", "/tmp/go-build1/b001/x.test", "/work"}
	if !slices.Equal(got, want) {
		t.Errorf("held = %v, want %v", got, want)
	}
	if got := held(root, 43); len(got) != 0 {
		t.Errorf("a missing process holds %v", got)
	}
}

func TestNamedFindsTheCallersProcessesByComm(t *testing.T) {
	root := t.TempDir()
	const goComm = "go\n"
	for pid, comm := range map[string]string{"10": goComm, "11": "gopls\n", "12": goComm, "self": goComm} {
		if err := os.MkdirAll(filepath.Join(root, pid), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, pid, "comm"), []byte(comm), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := namedAt(root, "go", os.Getuid())
	slices.Sort(got)
	if !slices.Equal(got, []int{10, 12}) {
		t.Errorf("namedAt = %v, want [10 12]", got)
	}
	if got := namedAt(root, "go", os.Getuid()+1); len(got) != 0 {
		t.Errorf("namedAt another uid = %v, want none", got)
	}
}
