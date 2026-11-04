package proc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestReadAtLongUptimeAndCPUTime(t *testing.T) {
	for _, tt := range []struct {
		name  string
		ticks int64
		want  time.Duration
	}{
		{"before overflow boundary", 9223372036, 92233720*time.Second + 360*time.Millisecond},
		{"at overflow boundary", 9223372037, 92233720*time.Second + 370*time.Millisecond},
		{"reported counter", 10000000001, 100000000*time.Second + 10*time.Millisecond},
		{"largest representable counter", 922337203685, 9223372036*time.Second + 850*time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "42"), 0o750); err != nil {
				t.Fatal(err)
			}
			// Split CPU ticks across utime and stime to exercise their sum.
			stat := fmt.Sprintf("42 (worker) S 1 42 42 0 -1 0 0 0 0 0 %d 1 0 0 20 0 1 0 %d 4096 1\n", tt.ticks-1, tt.ticks)
			for name, data := range map[string]string{
				"stat":       "btime 1700000000\n",
				"42/stat":    stat,
				"42/cmdline": "worker\x00",
			} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			tab, err := ReadAt(root)
			if err != nil {
				t.Fatal(err)
			}
			p := tab.Process(42)
			if p == nil {
				t.Fatal("process missing")
			}
			if p.CPU != tt.want {
				t.Errorf("CPU = %v, want %v", p.CPU, tt.want)
			}
			wantStart := time.Unix(1700000000, 0).Add(tt.want)
			if !p.Start.Equal(wantStart) {
				t.Errorf("Start = %v, want %v", p.Start, wantStart)
			}
			if p.StartTicks != tt.ticks {
				t.Errorf("StartTicks = %d, want %d", p.StartTicks, tt.ticks)
			}
		})
	}
}

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
	want := []string{"/dev/null", "/tmp/claude-1000/p/s/scratchpad/out.log", "/tmp/claude-1000/p/s/scratchpad/out.log (deleted)", "/tmp/go-build1/b001/x.test", "/work"}
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

func TestMissingReason(t *testing.T) {
	tb := &Table{readErrors: map[int]error{
		1: &os.PathError{Op: "open", Path: "private", Err: os.ErrPermission},
		2: os.ErrNotExist,
		3: errors.New("malformed stat"),
	}}
	for pid, want := range map[int]string{
		1: "not visible to the caller (permission denied)",
		2: "not found or not visible in the caller's process namespace",
		3: "could not read process information",
		4: "not found or not visible in the caller's process namespace",
	} {
		if got := tb.MissingReason(pid); got != want {
			t.Errorf("PID %d: %q, want %q", pid, got, want)
		}
	}
}

func TestReadRetainsProcessReadFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("btime 100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "42"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "42", "stat"), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	tb, err := ReadAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if tb.Process(42) != nil || tb.MissingReason(42) != "could not read process information" {
		t.Fatalf("unreadable process: %+v", tb)
	}
}

func TestReadProcessWithoutCmdlinePermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "42")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := "42 (sleep) S 1 42 42 0 -1 0 0 0 0 0 0 0 0 0 0 0 1 0 100 0 1"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte("private"), 0o000); err != nil {
		t.Fatal(err)
	}
	p, err := readProcess(root, 42, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if p.Comm != "sleep" || len(p.Args) != 0 {
		t.Fatalf("process = %+v", p)
	}
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("btime 100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "stat"), 0); err != nil {
		t.Fatal(err)
	}
	tb, err := ReadAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if tb.Process(42) != nil || tb.MissingReason(42) != "not visible to the caller (permission denied)" {
		t.Fatalf("permission failure: %+v", tb)
	}
}

func TestBootTimeAfterLongInterruptLine(t *testing.T) {
	for name, ending := range map[string]string{"with_final_newline": "\n", "without_final_newline": ""} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			raw := "cpu  1 0 1 1 0 0 0 0 0 0\nintr 0" + strings.Repeat(" 0", 40000) + "\nbtime 1700000000" + ending
			if err := os.WriteFile(filepath.Join(root, "stat"), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			want := time.Unix(1700000000, 0)
			if got, err := bootTime(root); err != nil || !got.Equal(want) {
				t.Fatalf("bootTime = %v, %v; want %v", got, err, want)
			}
			if _, err := ReadAt(root); err != nil {
				t.Fatalf("ReadAt: %v", err)
			}
		})
	}
}

// readOp is the os.PathError operation of a failed read.
const readOp = "read"

func TestBootTimeErrors(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"missing", "cpu 1 2 3\n", "no btime in /proc/stat"},
		{"malformed", "btime invalid\n", "invalid syntax"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "stat"), []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := bootTime(root); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("bootTime error = %v; want %q", err, tc.want)
			}
		})
	}
	t.Run("read_failure", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "stat"), 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := bootTime(root)
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) || pathErr.Op != readOp {
			t.Fatalf("bootTime error = %v; want underlying read error", err)
		}
	})
}
