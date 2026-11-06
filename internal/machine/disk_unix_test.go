//go:build unix

package machine

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestDiskUsage(t *testing.T) {
	const fixturePath = "/fixture"

	for _, tc := range []struct {
		name string
		stat unix.Statfs_t
		want Disk
	}{
		{
			name: "case 0: reserved blocks are unavailable to an unprivileged writer",
			stat: unix.Statfs_t{Bsize: 4096, Blocks: 1024, Bfree: 512, Bavail: 256},
			want: Disk{Path: fixturePath, UsedMiB: 2, FreeMiB: 1, TotalMiB: 4},
		},
		{
			name: "case 1: only reserved space remains",
			stat: unix.Statfs_t{Bsize: 4096, Blocks: 1024, Bfree: 512},
			want: Disk{Path: fixturePath, UsedMiB: 2, FreeMiB: 0, TotalMiB: 4},
		},
		{
			name: "case 2: fractional MiB is truncated",
			stat: unix.Statfs_t{Bsize: 4096, Blocks: 1025, Bfree: 513, Bavail: 511},
			want: Disk{Path: fixturePath, UsedMiB: 2, FreeMiB: 1, TotalMiB: 4},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := diskUsage(fixturePath, &tc.stat); got != tc.want {
				t.Errorf("disk usage = %+v, want %+v", got, tc.want)
			}
		})
	}
}
