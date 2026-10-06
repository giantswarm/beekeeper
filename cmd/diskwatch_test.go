package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/proc"
)

const gibMiB = 1024

func diskWatcher(out *bytes.Buffer, writes map[int]int64) *watcher {
	cfg := &config.Config{Watch: config.Watch{DiskFillWithin: config.Duration{Duration: 2 * time.Hour}}}
	w := &watcher{app: &app{out: out, cfg: cfg}, last: map[string]time.Time{}}
	w.disk.readWrites = func(pid int) (int64, error) {
		b, ok := writes[pid]
		if !ok {
			return 0, errUnreadable
		}
		return b, nil
	}
	return w
}

var errUnreadable = errors.New("permission denied")

// A disk of 1.9 TiB that loses 2.4 GiB a minute from 24 GiB free, written
// by one session's crane: LOW DISK at once, DISK FILLING naming crane and
// the session once two minutes of samples show the rate, DISK NEARLY FULL
// under 1 % free, each said once.
func TestSampleDiskSaysTheFillAndItsWriter(t *testing.T) {
	var out bytes.Buffer
	writes := map[int]int64{20: 0}
	w := diskWatcher(&out, writes)
	tab := named(table(
		&proc.Process{PID: 10, Args: []string{claudeComm}, StartTicks: 1},
		&proc.Process{PID: 20, PPID: 10, Args: []string{"crane", "export"}, StartTicks: 7},
		&proc.Process{PID: 30, PPID: 1, Args: []string{"dockerd"}, StartTicks: 2},
	))
	owners := map[int]string{10: "Board pull 210"}
	start := time.Date(2026, 10, 6, 12, 56, 0, 0, time.UTC)
	total := 1900 * gibMiB
	for i := range 13 {
		now := start.Add(time.Duration(i) * 30 * time.Second)
		lost := int64(i) * 1229 << 20 // 1.2 GiB per 30s
		writes[20] = lost
		w.sampleDisk(now, machine.Disk{FreeMiB: 24*gibMiB - int(lost>>20), TotalMiB: total}, tab, owners)
	}
	got := out.String()
	for _, want := range []string{
		"LOW DISK: / 24 GiB free",
		"DISK FILLING: / loses 2.4 GiB/min, 19 GiB free, full in ~7 min; written over the last 2m0s: top: crane 4.8 GiB; sessions: \"Board pull 210\" 4.8 GiB",
		"DISK NEARLY FULL: / 17 GiB free of 1900 GiB",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("watch output lacks %q:\n%s", want, got)
		}
	}
	for _, once := range []string{"LOW DISK", "DISK FILLING", "DISK NEARLY FULL"} {
		if n := strings.Count(got, once); n != 1 {
			t.Errorf("%s said %d times, want once:\n%s", once, n, got)
		}
	}
}

// A slow fill that would take far longer than diskFillWithin is no DISK
// FILLING, and free space that holds steady ends one.
func TestSampleDiskIgnoresASlowFill(t *testing.T) {
	var out bytes.Buffer
	w := diskWatcher(&out, map[int]int64{})
	start := time.Now()
	for i := range 21 {
		free := 500*gibMiB - i*50 // 0.1 GiB/min
		w.sampleDisk(start.Add(time.Duration(i)*30*time.Second), machine.Disk{FreeMiB: free, TotalMiB: 1900 * gibMiB}, table(), nil)
	}
	if out.Len() != 0 {
		t.Errorf("a slow fill said:\n%s", out.String())
	}
}

// A fill no readable process wrote (a root daemon) says so, with the share
// of the loss it cannot attribute.
func TestWritersLineNamesTheUnreadableShare(t *testing.T) {
	dw := diskWatch{readWrites: func(int) (int64, error) { return 0, errUnreadable }}
	start := time.Now()
	tab := table(&proc.Process{PID: 30, Comm: "dockerd", StartTicks: 2})
	dw.rebase(start, 50*gibMiB, tab)
	got := dw.writersLine(start.Add(5*time.Minute), 40*gibMiB, tab, nil)
	want := "written over the last 5m0s: by no process the watch can read; 10.0 GiB of the 10.0 GiB lost by processes the watch cannot read (another user's, such as dockerd, or ended ones)"
	if got != want {
		t.Errorf("writersLine =\n %q\nwant\n %q", got, want)
	}
}

// The baseline is replaced once the newer one is a window old, so the
// writers span between one and two windows; a reused PID counts from zero.
func TestRebaseKeepsTheOlderBaseline(t *testing.T) {
	writes := map[int]int64{1: 100}
	dw := diskWatch{readWrites: func(pid int) (int64, error) { return writes[pid], nil }}
	start := time.Now()
	tab := table(&proc.Process{PID: 1, Comm: "go", StartTicks: 5})
	dw.rebase(start, 100, tab)
	dw.rebase(start.Add(diskWindow-time.Second), 100, tab)
	if dw.base.at != start || dw.next.at != start {
		t.Fatalf("rebased before a window passed")
	}
	dw.rebase(start.Add(diskWindow), 100, tab)
	if dw.base.at != start || dw.next.at != start.Add(diskWindow) {
		t.Fatalf("base %v next %v, want the first kept and a new next", dw.base.at, dw.next.at)
	}
	tab.ByPID[1] = &proc.Process{PID: 1, Comm: "go", StartTicks: 9}
	writes[1] = 3 << 30
	if got := dw.writersLine(start.Add(diskWindow), 100, tab, nil); !strings.Contains(got, "top: go 3.0 GiB") {
		t.Errorf("a reused PID's writes = %q, want all 3 GiB", got)
	}
}
