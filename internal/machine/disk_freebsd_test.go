package machine

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestDiskFromStatfsNegativeAvailability(t *testing.T) {
	s := unix.Statfs_t{Bsize: 4096, Blocks: 2560, Bfree: 1024, Bavail: -512}
	want := Disk{Path: "synthetic", UsedMiB: 6, FreeMiB: 0, TotalMiB: 10}
	if got := diskFromStatfs("synthetic", s); got != want {
		t.Fatalf("diskFromStatfs() = %+v, want %+v", got, want)
	}
}
