package proc

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/diskprune"
)

const zombieLeaderHelper = "BEEKEEPER_TEST_ZOMBIE_LEADER_HOLD"

// The helper's main goroutine must run on the leader thread so that a raw
// exit of that thread leaves a zombie leader whose other threads live on.
func init() {
	if os.Getenv(zombieLeaderHelper) != "" {
		runtime.LockOSThread()
	}
}

func TestMain(m *testing.M) {
	if held := os.Getenv(zombieLeaderHelper); held != "" {
		zombieLeader(held)
	}
	os.Exit(m.Run())
}

// zombieLeader holds a file open on a live thread, then ends only the
// thread-group leader.
func zombieLeader(held string) {
	f, err := os.Open(filepath.Clean(held)) //nolint:gosec // the parent test's own temp file
	if err != nil {
		os.Exit(2)
	}
	go func() {
		runtime.LockOSThread()
		for {
			time.Sleep(time.Hour)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	_, _ = os.Stdout.WriteString("ready\n")
	_ = f // the descriptor stays open in the shared table
	for {
		_, _, _ = syscall.RawSyscall(syscall.SYS_EXIT, 0, 0, 0)
	}
}

func procState(t *testing.T, pid int) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), fixtureStatus))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(raw)) {
		if v, ok := strings.CutPrefix(line, "State:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func waitZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.HasPrefix(procState(t, pid), "Z") {
		if time.Now().After(deadline) {
			t.Fatalf("process %d did not become a zombie: %s", pid, procState(t, pid))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A zombie child of a live process sits next to prune candidates: a plain
// exited child holds nothing, a zombie leader holds what its live threads
// hold, and neither fails the scan.
func TestUserReferencesZombieChildrenNextToPruneCandidates(t *testing.T) {
	if _, err := os.Stat("/proc/self/task"); err != nil {
		t.Skip("Linux proc is unavailable")
	}
	base := t.TempDir()
	held, idle := filepath.Join(base, "held"), filepath.Join(base, "idle")
	for _, d := range []string{held, idle} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(held, "data")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	exited := exec.Command("true")
	if err := exited.Start(); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = exited.Wait() })
	waitZombie(t, exited.Process.Pid)

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	leader := exec.Command(self, "-test.run=^$") //nolint:gosec // this test binary, re-run as the helper
	leader.Env = append(os.Environ(), zombieLeaderHelper+"="+file)
	out, err := leader.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = leader.Process.Kill(); _ = leader.Wait() })
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("helper did not start: %q %v", line, err)
	}
	waitZombie(t, leader.Process.Pid)

	pids := []string{strconv.Itoa(os.Getpid()), strconv.Itoa(exited.Process.Pid), strconv.Itoa(leader.Process.Pid)}
	files := referenceFiles()
	files.readDir = func(path string) ([]os.DirEntry, error) {
		entries, err := os.ReadDir(path)
		if path != procRoot || err != nil {
			return entries, err
		}
		return slices.DeleteFunc(entries, func(e os.DirEntry) bool { return !slices.Contains(pids, e.Name()) }), nil
	}
	refs, _, err := userReferencesAt(procRoot, os.Getuid(), files)
	if err != nil {
		t.Fatalf("zombie children failed the scan: %v", err)
	}
	if !diskprune.InUse(held, refs) {
		t.Error("a zombie leader's live thread's open file was not seen")
	}
	if diskprune.InUse(idle, refs) {
		t.Error("an unreferenced candidate looked in use")
	}
}
