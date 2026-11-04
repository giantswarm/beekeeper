package guard

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestRunCoreLimits(t *testing.T) {
	saved := plat.Capper
	t.Cleanup(func() { plat.Capper = saved })
	for _, brokered := range []bool{false, true} {
		name := "plain"
		if brokered {
			name = "brokered"
		}
		t.Run(name, func(t *testing.T) {
			host, broker := &fakeCapper{down: brokered}, &fakeCapper{}
			plat.Capper = host
			dir := t.TempDir()
			t.Setenv("CLAUDE_CODE_SESSION_ID", name)
			o := Options{Max: "1M", Swap: "0", SlotDir: t.TempDir(), Slots: 1, Stderr: io.Discard}
			if brokered {
				o.Sandbox = broker
			}
			// Check both limits in the actual launched command, and abort a child
			// in an isolated directory to check that it leaves no core file.
			script := `test "$(ulimit -S -c)" = 0 && test "$(ulimit -H -c)" = 0 || exit 42
cd "$1" || exit 43
sh -c 'kill -ABRT $$'
test -z "$(find . -name 'core*' -print)"`
			if rc := Run(o, []string{"sh", "-c", script, "test", dir}); rc != 0 {
				t.Fatalf("run exit %d", rc)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(filepath.Base(entry.Name()), "core") {
					t.Fatalf("core file: %s", entry.Name())
				}
			}
			if brokered && len(broker.ran) != 1 {
				t.Fatal("broker command was not launched")
			}
		})
	}
}

func TestCoreCommandOptIn(t *testing.T) {
	argv := []string{"sh", "-c", "printf '%s' \"$1\"", "test", "argument with spaces"}
	if got := coreCommand(argv, true); !slices.Equal(got, argv) {
		t.Fatalf("--core changed the inherited command: %q", got)
	}
}

func TestCoreCommandPreservesLimitsAndArguments(t *testing.T) {
	var inherited syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &inherited); err != nil {
		t.Fatal(err)
	}
	for _, core := range []bool{false, true} {
		argv := coreCommand([]string{"sh", "-c", "printf '%s\\n' \"$1\"; ulimit -S -c; ulimit -H -c", "test", "argument with spaces; $HOME"}, core)
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput() //nolint:gosec // owned test command
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		limits := syscall.Rlimit{}
		if core {
			limits = inherited
		}
		limitText := func(n uint64) string {
			if n == ^uint64(0) {
				return "unlimited"
			}
			return fmt.Sprint(n / 1024)
		}
		want := "argument with spaces; $HOME\n" + limitText(limits.Cur) + "\n" + limitText(limits.Max) + "\n"
		if string(out) != want {
			t.Fatalf("core=%v: got %q, want %q", core, out, want)
		}
	}
}
