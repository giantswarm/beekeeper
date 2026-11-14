package cmd

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// diskWindow is how far back the fill rate of / looks and how long a
// baseline of the processes' writes stands; minDiskSpan is the shortest
// span the rate is measured over.
const (
	DiskWindow  = 10 * time.Minute
	minDiskSpan = 2 * time.Minute
)

// diskSample is one machine sample's free space on /.
type diskSample struct {
	at      time.Time
	freeMiB int
}

// ioMark is a process's write_bytes when a baseline was taken; start tells
// a reused PID from the process the baseline saw.
type ioMark struct {
	start int64
	bytes int64
}

// ioBaseline is every readable process's write_bytes and the free space on
// / at one moment: the writers of a fill are measured against it.
type ioBaseline struct {
	At      time.Time
	freeMiB int
	marks   map[int]ioMark
}

// DiskWatch is the watch's memory of / between machine samples.
type DiskWatch struct {
	samples   []diskSample
	Paths     []diskPath
	pathsAt   time.Time
	ReadPaths func(context.Context) []diskPath
	// base is the baseline the writers are measured against, next the one
	// that replaces it once it is a diskWindow old: the writers always span
	// between one and two windows.
	Base, Next *ioBaseline
	// readWrites reads a process's write_bytes; nil is proc.WriteBytes.
	ReadWrites func(pid int) (int64, error)
}

// SampleDisk says LOW DISK under watch.diskMinMiB, DISK NEARLY FULL under
// watch.diskCriticalMiB, and DISK FILLING while / would run full within
// watch.diskFillWithin at the rate its free space fell over the last
// diskWindow, with the commands and sessions that wrote most. Only the
// free space under the floor is the emergency that holds starts (the
// reading's Low); the fill prediction triggers an early disk step and
// nothing else: a rate is a forecast, and it is wrong whenever it does
// not last.
func (w *Watcher) SampleDisk(now time.Time, d machine.Disk, t *proc.Table, owners map[int]string) {
	if w.Disk.pathsAt.IsZero() || now.Sub(w.Disk.pathsAt) >= minDiskSpan {
		read := w.Disk.ReadPaths
		if read == nil {
			read = func(ctx context.Context) []diskPath { return readDiskPaths(ctx, w.Host.diskPathRoots()) }
		}
		paths := read(w.Host.context())
		diskPathGrowth(paths, w.Disk.Paths)
		w.Disk.Paths = paths
		w.Disk.pathsAt = now
	}
	th := w.Host.cfg.Watch
	low := d.FreeMiB < th.DiskMin(d.TotalMiB)
	w.Check("disk", low, "LOW DISK: / %d GiB free", d.FreeMiB/1024)
	w.Check("diskfull", d.FreeMiB < th.DiskCritical(d.TotalMiB), "DISK NEARLY FULL: / %d GiB free of %d GiB", d.FreeMiB/1024, d.TotalMiB/1024)

	w.Disk.Rebase(now, d.FreeMiB, t)
	perMin := w.Disk.fillRate(now, d.FreeMiB)
	filling := perMin > 0 && time.Duration(float64(d.FreeMiB)/perMin*float64(time.Minute)) < th.DiskFillWithin.Duration
	line := ""
	if filling && !w.IsActive("diskfill") {
		line = fmt.Sprintf("DISK FILLING: / loses %.1f GiB/min, %d GiB free, full in ~%d min; %s",
			perMin/1024, d.FreeMiB/1024, int(float64(d.FreeMiB)/perMin), w.Disk.WritersLine(now, d.FreeMiB, t, owners))
	}
	w.Check("diskfill", filling, "%s", line)
	w.PublishDisk(capacity.WatchDiskReading{Low: low, Filling: filling, Writers: w.Disk.WritersLine(now, d.FreeMiB, t, owners), FreeMiB: d.FreeMiB, MinMiB: th.DiskMin(d.TotalMiB)})
	w.DiskPrune(now, low || filling)
}

// fillRate records free and returns how many MiB a minute / lost over the
// last diskWindow; 0 while the samples span less than minDiskSpan or free
// space did not fall.
func (dw *DiskWatch) fillRate(now time.Time, freeMiB int) float64 {
	dw.samples = append(dw.samples, diskSample{now, freeMiB})
	i := 0
	for i < len(dw.samples)-1 && now.Sub(dw.samples[i].at) > DiskWindow {
		i++
	}
	dw.samples = dw.samples[i:]
	first := dw.samples[0]
	span := now.Sub(first.at)
	if span < minDiskSpan || freeMiB >= first.freeMiB {
		return 0
	}
	return float64(first.freeMiB-freeMiB) / span.Minutes()
}

// Rebase takes a new baseline of the processes' writes once the newer one
// is a diskWindow old, keeping the one before it to measure against.
func (dw *DiskWatch) Rebase(now time.Time, freeMiB int, t *proc.Table) {
	if t == nil || dw.Next != nil && now.Sub(dw.Next.At) < DiskWindow {
		return
	}
	dw.Base, dw.Next = dw.Next, &ioBaseline{At: now, freeMiB: freeMiB, marks: dw.read(t)}
	if dw.Base == nil {
		dw.Base = dw.Next
	}
}

// read is the write_bytes of every process of t the watch can read.
func (dw *DiskWatch) read(t *proc.Table) map[int]ioMark {
	readWrites := dw.ReadWrites
	if readWrites == nil {
		readWrites = proc.WriteBytes
	}
	marks := make(map[int]ioMark, len(t.ByPID))
	for pid, p := range t.ByPID {
		if b, err := readWrites(pid); err == nil {
			marks[pid] = ioMark{start: p.StartTicks, bytes: b}
		}
	}
	return marks
}

// diskNames is how many commands and sessions a DISK FILLING line names.
const diskNames = 3

// WritersLine names the commands and the sessions whose running processes
// wrote most since the baseline, and says how much of the space / lost
// since then no readable process accounts for: another user's processes
// (a root daemon such as dockerd or containerd), or ones that already ended.
func (dw *DiskWatch) WritersLine(now time.Time, freeMiB int, t *proc.Table, owners map[int]string) string {
	if dw.Base == nil || t == nil {
		return "writers unknown" + dw.pathLine()
	}
	comms, sessions := map[string]int64{}, map[string]int64{}
	var total int64
	for pid, m := range dw.read(t) {
		was := dw.Base.marks[pid]
		if was.start != m.start {
			was = ioMark{} // started since the baseline
		}
		n := m.bytes - was.bytes
		if n <= 0 {
			continue
		}
		total += n
		comms[t.ByPID[pid].Comm] += n
		if s := owner(t, pid, owners); s != "" {
			sessions[fmt.Sprintf("%q", s)] += n
		}
	}
	span := now.Sub(dw.Base.At).Round(time.Minute)
	line := fmt.Sprintf("written over the last %s: ", span)
	if total == 0 {
		line += "by no process the watch can read"
	} else {
		line += "top: " + bytesLine(comms, diskNames)
		if len(sessions) > 0 {
			line += "; sessions: " + bytesLine(sessions, diskNames)
		}
	}
	if lost := int64(dw.Base.freeMiB-freeMiB) << 20; lost > 2*total {
		line += fmt.Sprintf("; %s of the %s lost by processes the watch cannot read (another user's, such as dockerd, or ended ones)",
			gibStr(lost-total), gibStr(lost))
	}
	return line + dw.pathLine()
}

// bytesLine is the n largest of by, largest first, with their sizes.
func bytesLine(by map[string]int64, n int) string {
	names := slices.SortedFunc(maps.Keys(by), func(a, b string) int {
		return cmp.Or(cmp.Compare(by[b], by[a]), cmp.Compare(a, b))
	})
	parts := make([]string, 0, n)
	for _, name := range names[:min(n, len(names))] {
		parts = append(parts, name+" "+gibStr(by[name]))
	}
	return strings.Join(parts, ", ")
}

func (dw *DiskWatch) pathLine() string {
	paths := topDiskPaths(dw.Paths, true, 1)
	if len(paths) == 0 {
		return ""
	}
	return "; fastest-growing path: " + diskPathText(paths[0])
}
