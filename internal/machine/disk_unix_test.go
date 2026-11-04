//go:build unix

package machine

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestDiskFromStatfs(t *testing.T) {
	for _, tt := range []struct {
		name string
		stat unix.Statfs_t
		want Disk
	}{
		{
			name: "reserved blocks",
			stat: unix.Statfs_t{Bsize: 4096, Blocks: 2560, Bfree: 1024, Bavail: 512},
			want: Disk{Path: "synthetic", UsedMiB: 6, FreeMiB: 2, TotalMiB: 10},
		},
		{
			name: "exhausted availability",
			stat: unix.Statfs_t{Bsize: 4096, Blocks: 2560, Bfree: 1024, Bavail: 0},
			want: Disk{Path: "synthetic", UsedMiB: 6, FreeMiB: 0, TotalMiB: 10},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := diskFromStatfs("synthetic", tt.stat); got != tt.want {
				t.Fatalf("diskFromStatfs() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
