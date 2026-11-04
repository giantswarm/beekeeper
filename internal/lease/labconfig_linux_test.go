package lease

import (
	"errors"
	"flag"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"syscall"
	"testing"
)

// A failed notice write must leave either the previous complete notice or
// no notice. A partial JSON file would refuse every subsequent lab claim.
// The file-size limit applies only to the disposable helper subprocess.
func TestLeaveLabChangeWriteFailure(t *testing.T) {
	if args := flag.Args(); len(args) == 1 {
		signal.Ignore(syscall.SIGXFSZ)
		var limit syscall.Rlimit
		if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
			t.Fatal(err)
		}
		limit.Cur = 1
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
			t.Fatal(err)
		}
		err := Dir(args[0]).LeaveLabChange("lab", LabChange{By: "fixture", Keys: []string{"platform.valuesFiles"}})
		if !errors.Is(err, syscall.EFBIG) {
			t.Fatalf("LeaveLabChange = %v, want file-size limit failure", err)
		}
		return
	}
	for _, exists := range []bool{false, true} {
		name := "new notice"
		if exists {
			name = "existing notice"
		}
		t.Run(name, func(t *testing.T) {
			d := Dir(t.TempDir())
			old := LabChange{By: "previous fixture", Keys: []string{"platform.chartVersion"}, Why: "keep the previous proof"}
			if exists {
				if err := d.LeaveLabChange("lab", old); err != nil {
					t.Fatal(err)
				}
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=^TestLeaveLabChangeWriteFailure$", "--", string(d)) //nolint:gosec // this test binary, isolated fixture directory
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("short-write helper: %v: %s", err, out)
			}
			got, err := d.LabChangeOf("lab")
			if err != nil {
				t.Fatalf("failed write left an unreadable notice: %v", err)
			}
			if exists && !reflect.DeepEqual(got, &old) || !exists && got != nil {
				t.Fatalf("notice after failed write = %+v; previous notice exists: %t", got, exists)
			}
			entries, err := os.ReadDir(string(d))
			if err != nil {
				t.Fatal(err)
			}
			wantFiles := 0
			if exists {
				wantFiles = 1
			}
			if len(entries) != wantFiles {
				t.Fatalf("failed write left %d files, want %d", len(entries), wantFiles)
			}
			next := LabChange{By: "next fixture", Keys: []string{"clusterName"}, Why: "retry after recovery"}
			if err := d.LeaveLabChange("lab", next); err != nil {
				t.Fatal(err)
			}
			if got, err := d.LabChangeOf("lab"); err != nil || !reflect.DeepEqual(got, &next) {
				t.Fatalf("retried notice = %+v, %v", got, err)
			}
		})
	}
}
