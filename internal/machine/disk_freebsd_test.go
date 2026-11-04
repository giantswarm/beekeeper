package machine

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestDiskUsageNegativeAvailability(t *testing.T) {
	stat := unix.Statfs_t{Bsize: 4096, Blocks: 1024, Bfree: 512, Bavail: -256}
	want := Disk{Path: "/fixture", UsedMiB: 2, FreeMiB: 0, TotalMiB: 4}
	if got := diskUsage("/fixture", &stat); got != want {
		t.Errorf("exhausted available blocks = %+v, want %+v", got, want)
	}
}
