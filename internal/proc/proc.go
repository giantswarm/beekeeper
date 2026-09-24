// Package proc reads the Linux process table from /proc once and answers the
// questions beekeeper asks of it: who is whose parent, which session a
// process belongs to, how long it has run. Reading /proc directly instead of
// grepping `ps` output means a pattern never matches the command that looks
// for it.
package proc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// clockTicks is USER_HZ, 100 on every Linux architecture Go supports.
const clockTicks = 100

// Process is one entry of the table.
type Process struct {
	PID   int
	PPID  int
	Comm  string
	Args  []string
	Start time.Time
	// CPU is the processor time it has burned (user and system).
	CPU time.Duration
	// RSSKiB is its resident memory, file-backed pages included.
	RSSKiB int
}

// Cmdline is the argument vector joined by spaces.
func (p *Process) Cmdline() string { return strings.Join(p.Args, " ") }

// Elapsed is how long the process has run at now.
func (p *Process) Elapsed(now time.Time) time.Duration { return now.Sub(p.Start) }

// Table is a snapshot of the process table.
type Table struct {
	ByPID    map[int]*Process
	children map[int][]int
}

// Read scans /proc.
func Read() (*Table, error) {
	boot, err := bootTime()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	t := &Table{ByPID: map[int]*Process{}, children: map[int][]int{}}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		p, err := readProcess(pid, boot)
		if err != nil {
			continue // exited while scanning
		}
		t.ByPID[pid] = p
		t.children[p.PPID] = append(t.children[p.PPID], pid)
	}
	for _, c := range t.children {
		slices.Sort(c)
	}
	return t, nil
}

func readProcess(pid int, boot time.Time) (*Process, error) {
	dir := filepath.Join("/proc", strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Clean(filepath.Join(dir, "stat")))
	if err != nil {
		return nil, err
	}
	st, err := parseStat(stat)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(dir, "cmdline")))
	if err != nil {
		return nil, err
	}
	return &Process{
		PID:    pid,
		PPID:   st.ppid,
		Comm:   st.comm,
		Args:   splitNul(raw),
		Start:  boot.Add(ticks(st.start)),
		CPU:    ticks(st.cpu),
		RSSKiB: int(st.rssPages * int64(os.Getpagesize()) / 1024),
	}, nil
}

func ticks(n int64) time.Duration { return time.Duration(n) * time.Second / clockTicks }

// stat is the part of /proc/<pid>/stat beekeeper reads; times in clock ticks.
type stat struct {
	comm     string
	ppid     int
	cpu      int64 // utime + stime
	start    int64 // since boot
	rssPages int64
}

// parseStat reads /proc/<pid>/stat. comm is in parentheses and may itself
// contain spaces and parentheses, so the fields after it are found from the
// last ')'.
func parseStat(b []byte) (stat, error) {
	open, end := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return stat{}, errors.New("malformed stat")
	}
	f := strings.Fields(string(b[end+1:]))
	// f[0] is field 3 (state): field n is f[n-3]. ppid is field 4, utime
	// and stime 14 and 15, starttime 22, rss 24.
	if len(f) < 22 {
		return stat{}, errors.New("short stat")
	}
	n := map[int]int64{}
	for _, i := range []int{4, 14, 15, 22, 24} {
		v, err := strconv.ParseInt(f[i-3], 10, 64)
		if err != nil {
			return stat{}, err
		}
		n[i] = v
	}
	return stat{comm: string(b[open+1 : end]), ppid: int(n[4]), cpu: n[14] + n[15], start: n[22], rssPages: n[24]}, nil
}

func splitNul(b []byte) []string {
	b = bytes.TrimRight(b, "\x00")
	if len(b) == 0 {
		return nil
	}
	parts := bytes.Split(b, []byte{0})
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(p)
	}
	return out
}

func bootTime() (time.Time, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return time.Time{}, fmt.Errorf("reading the process table needs Linux /proc: %w", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			s, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(s, 0), nil
		}
	}
	return time.Time{}, errors.New("no btime in /proc/stat")
}

// Ancestors returns the parent chain of pid, nearest first.
func (t *Table) Ancestors(pid int) []*Process {
	var out []*Process
	seen := map[int]bool{pid: true}
	for p := t.ByPID[pid]; p != nil; {
		pp := t.ByPID[p.PPID]
		if pp == nil || seen[pp.PID] {
			break
		}
		seen[pp.PID] = true
		out = append(out, pp)
		p = pp
	}
	return out
}

// Descendants returns every process below pid, depth first.
func (t *Table) Descendants(pid int) []*Process {
	var out []*Process
	var walk func(int)
	walk = func(p int) {
		for _, c := range t.children[p] {
			out = append(out, t.ByPID[c])
			walk(c)
		}
	}
	walk(pid)
	return out
}

// Children returns the direct children of pid.
func (t *Table) Children(pid int) []*Process {
	out := make([]*Process, 0, len(t.children[pid]))
	for _, c := range t.children[pid] {
		out = append(out, t.ByPID[c])
	}
	return out
}

// Environ returns the environment of pid (readable for the caller's own
// processes).
func Environ(pid int) (map[string]string, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, kv := range splitNul(raw) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env, nil
}

// Cwd returns the working directory of pid.
func Cwd(pid int) string {
	d, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	if err != nil {
		return ""
	}
	return d
}

// AnonKiB returns the anonymous resident memory of pid in KiB: what killing
// it would give back.
func AnonKiB(pid int) int {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "RssAnon:"); ok {
			n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(v), " kB"))
			return n
		}
	}
	return 0
}

// UID returns the real user id pid runs as, or -1 when it cannot be read.
func UID(pid int) int {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return -1
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "Uid:"); ok {
			if f := strings.Fields(v); len(f) > 0 {
				n, err := strconv.Atoi(f[0])
				if err == nil {
					return n
				}
			}
		}
	}
	return -1
}

// Cgroup returns the cgroup v2 path of pid ("/user.slice/…"), or "".
func Cgroup(pid int) string {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return p
		}
	}
	return ""
}
