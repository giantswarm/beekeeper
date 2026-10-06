package cmd

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// diskWindow is how far back the fill rate of / looks and how long a
// baseline of the processes' writes stands; minDiskSpan is the shortest
// span the rate is measured over.
const (
	diskWindow  = 10 * time.Minute
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
	at      time.Time
	freeMiB int
	marks   map[int]ioMark
}

// diskWatch is the watch's memory of / between machine samples.
type diskWatch struct {
	samples []diskSample
	// base is the baseline the writers are measured against, next the one
	// that replaces it once it is a diskWindow old: the writers always span
	// between one and two windows.
	base, next *ioBaseline
	// readWrites reads a process's write_bytes; nil is proc.WriteBytes.
	readWrites func(pid int) (int64, error)
}

// sampleDisk says LOW DISK under watch.diskMinMiB, DISK NEARLY FULL under
// watch.diskCriticalMiB, and DISK FILLING while / would run full within
// watch.diskFillWithin at the rate its free space fell over the last
// diskWindow, with the commands and sessions that wrote most.
func (w *watcher) sampleDisk(now time.Time, d machine.Disk, t *proc.Table, owners map[int]string) {
	th := w.cfg.Watch
	w.check("disk", d.FreeMiB < th.DiskMin(d.TotalMiB), "LOW DISK: / %d GiB free", d.FreeMiB/1024)
	w.check("diskfull", d.FreeMiB < th.DiskCritical(d.TotalMiB), "DISK NEARLY FULL: / %d GiB free of %d GiB", d.FreeMiB/1024, d.TotalMiB/1024)

	w.disk.rebase(now, d.FreeMiB, t)
	perMin := w.disk.fillRate(now, d.FreeMiB)
	filling := perMin > 0 && time.Duration(float64(d.FreeMiB)/perMin*float64(time.Minute)) < th.DiskFillWithin.Duration
	line := ""
	if filling && !w.isActive("diskfill") {
		line = fmt.Sprintf("DISK FILLING: / loses %.1f GiB/min, %d GiB free, full in ~%d min; %s",
			perMin/1024, d.FreeMiB/1024, int(float64(d.FreeMiB)/perMin), w.disk.writersLine(now, d.FreeMiB, t, owners))
	}
	w.check("diskfill", filling, "%s", line)
}

// fillRate records free and returns how many MiB a minute / lost over the
// last diskWindow; 0 while the samples span less than minDiskSpan or free
// space did not fall.
func (dw *diskWatch) fillRate(now time.Time, freeMiB int) float64 {
	dw.samples = append(dw.samples, diskSample{now, freeMiB})
	i := 0
	for i < len(dw.samples)-1 && now.Sub(dw.samples[i].at) > diskWindow {
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

// rebase takes a new baseline of the processes' writes once the newer one
// is a diskWindow old, keeping the one before it to measure against.
func (dw *diskWatch) rebase(now time.Time, freeMiB int, t *proc.Table) {
	if t == nil || dw.next != nil && now.Sub(dw.next.at) < diskWindow {
		return
	}
	dw.base, dw.next = dw.next, &ioBaseline{at: now, freeMiB: freeMiB, marks: dw.read(t)}
	if dw.base == nil {
		dw.base = dw.next
	}
}

// read is the write_bytes of every process of t the watch can read.
func (dw *diskWatch) read(t *proc.Table) map[int]ioMark {
	readWrites := dw.readWrites
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

// writersLine names the commands and the sessions whose running processes
// wrote most since the baseline, and says how much of the space / lost
// since then no readable process accounts for: another user's processes
// (a root daemon such as dockerd or containerd), or ones that already ended.
func (dw *diskWatch) writersLine(now time.Time, freeMiB int, t *proc.Table, owners map[int]string) string {
	if dw.base == nil || t == nil {
		return "writers unknown"
	}
	comms, sessions := map[string]int64{}, map[string]int64{}
	var total int64
	for pid, m := range dw.read(t) {
		was := dw.base.marks[pid]
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
	span := now.Sub(dw.base.at).Round(time.Minute)
	line := fmt.Sprintf("written over the last %s: ", span)
	if total == 0 {
		line += "by no process the watch can read"
	} else {
		line += "top: " + bytesLine(comms, diskNames)
		if len(sessions) > 0 {
			line += "; sessions: " + bytesLine(sessions, diskNames)
		}
	}
	if lost := int64(dw.base.freeMiB-freeMiB) << 20; lost > 2*total {
		line += fmt.Sprintf("; %s of the %s lost by processes the watch cannot read (another user's, such as dockerd, or ended ones)",
			gibStr(lost-total), gibStr(lost))
	}
	return line
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

// gibStr is a byte count in GiB with one decimal.
func gibStr(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }
