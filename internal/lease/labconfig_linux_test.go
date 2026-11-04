package lease

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"syscall"
	"testing"
)

// A file-size limit produces a real short write without filling storage or
// changing the test runner's limits. Only the disposable child sets the limit.
func TestLeaveLabChangeWriteFailure(t *testing.T) {
	if os.Getenv("BEEKEEPER_TEST_NOTICE_WRITE_FAILURE") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestLeaveLabChangeWriteFailure$", "-test.v") //nolint:gosec // reexecute this test binary with a fixed test selector
		cmd.Env = append(os.Environ(), "BEEKEEPER_TEST_NOTICE_WRITE_FAILURE=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("subprocess: %v\n%s", err, out)
		}
		return
	}
	signal.Ignore(syscall.SIGXFSZ)
	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
		t.Fatal(err)
	}
	for _, existing := range []bool{false, true} {
		name := "absent"
		if existing {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			d := Dir(t.TempDir())
			previous := LabChange{By: "previous-holder", Keys: []string{"platform.chartVersion"}}
			replacement := LabChange{By: "next-holder", Keys: []string{"platform.valuesFiles"}}
			var want *LabChange
			if existing {
				if err := d.LeaveLabChange("lab", previous); err != nil {
					t.Fatal(err)
				}
				want = &previous
			}
			limited := original
			limited.Cur = 1
			if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limited); err != nil {
				t.Fatal(err)
			}
			writeErr := d.LeaveLabChange("lab", replacement)
			if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(writeErr, syscall.EFBIG) {
				t.Fatalf("write error = %v, want EFBIG", writeErr)
			}
			got, err := d.LabChangeOf("lab")
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("notice after failure = %+v, %v; want %+v", got, err, want)
			}
			entries, err := os.ReadDir(string(d))
			if err != nil {
				t.Fatal(err)
			}
			wantEntries := 0
			if existing {
				wantEntries = 1
			}
			if len(entries) != wantEntries {
				t.Fatalf("files after failure = %v; want %d notice files and no temporary files", entries, wantEntries)
			}
			if err := d.LeaveLabChange("lab", replacement); err != nil {
				t.Fatal(err)
			}
			got, err = d.LabChangeOf("lab")
			if err != nil || !reflect.DeepEqual(got, &replacement) {
				t.Fatalf("notice after retry = %+v, %v", got, err)
			}
			info, err := os.Stat(d.changePath("lab"))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("notice permissions = %o, want 600", info.Mode().Perm())
			}
		})
	}
}
