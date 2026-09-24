// Package machine reads the numbers that decide whether the workstation
// survives the next hour: RAM and swap, memory pressure, the Claude Desktop
// cgroup, tmpfs and disk, build slots, kind clusters and the kernel's OOM
// kills. Linux only: /proc, cgroup v2 and the journal.
package machine

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// Mem is /proc/meminfo in MiB.
type Mem struct {
	TotalMiB     int `json:"totalMiB"`
	AvailableMiB int `json:"availableMiB"`
	SwapTotalMiB int `json:"swapTotalMiB"`
	SwapUsedMiB  int `json:"swapUsedMiB"`
	ShmemMiB     int `json:"shmemMiB"`
}

// ReadMem parses /proc/meminfo.
func ReadMem() (Mem, error) {
	f, err := os.Open(filepath.Clean("/proc/meminfo"))
	if err != nil {
		return Mem{}, err
	}
	defer func() { _ = f.Close() }()
	kb := map[string]int{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(v), " kB"))
		kb[k] = n
	}
	return Mem{
		TotalMiB:     kb["MemTotal"] / 1024,
		AvailableMiB: kb["MemAvailable"] / 1024,
		SwapTotalMiB: kb["SwapTotal"] / 1024,
		SwapUsedMiB:  (kb["SwapTotal"] - kb["SwapFree"]) / 1024,
		ShmemMiB:     kb["Shmem"] / 1024,
	}, sc.Err()
}

// ReadLoad returns the 1, 5 and 15 minute load averages.
func ReadLoad() ([3]float64, error) {
	var l [3]float64
	raw, err := os.ReadFile(filepath.Clean("/proc/loadavg"))
	if err != nil {
		return l, err
	}
	f := strings.Fields(string(raw))
	for i := 0; i < 3 && i < len(f); i++ {
		l[i], _ = strconv.ParseFloat(f[i], 64)
	}
	return l, nil
}

// ReadPSIFull60 returns memory pressure "full avg60" in percent: the share of
// the last minute in which every task stalled on memory.
func ReadPSIFull60() (float64, error) {
	raw, err := os.ReadFile(filepath.Clean("/proc/pressure/memory"))
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if !strings.HasPrefix(line, "full ") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(f, "avg60="); ok {
				return strconv.ParseFloat(v, 64)
			}
		}
	}
	return 0, nil
}

// Scope is the Claude Desktop cgroup: every session's CLI and MCP servers
// live in it, and systemd-oomd kills it whole.
type Scope struct {
	Path       string `json:"path"`
	CurrentMiB int    `json:"currentMiB"`
	AnonMiB    int    `json:"anonMiB"`
	SwapMiB    int    `json:"swapMiB"`
	High       string `json:"high"`
	Max        string `json:"max"`
	SwapMax    string `json:"swapMax"`
	OOMKills   int64  `json:"oomKills"`
	HighEvents int64  `json:"highEvents"`
}

// scopeGlob matches the desktop app's scope with or without a compositor
// prefix (app-com.anthropic.Claude-….scope, app-Hyprland-com.anthropic.Claude-….scope).
const scopeGlob = "/sys/fs/cgroup/user.slice/user-*.slice/user@*.service/app.slice/app-*com.anthropic.Claude*.scope"

// FindScope returns the largest Claude Desktop scope, or "" when none runs.
func FindScope() string {
	m, _ := filepath.Glob(scopeGlob)
	best, bestN := "", int64(-1)
	for _, p := range m {
		if n := readInt(filepath.Join(p, "memory.current")); n > bestN {
			best, bestN = p, n
		}
	}
	return best
}

// ReadScope reads the cgroup at path.
func ReadScope(path string) *Scope {
	s := &Scope{
		Path:       path,
		CurrentMiB: int(readInt(filepath.Join(path, "memory.current")) >> 20),
		SwapMiB:    int(readInt(filepath.Join(path, "memory.swap.current")) >> 20),
		High:       limit(filepath.Join(path, "memory.high")),
		Max:        limit(filepath.Join(path, "memory.max")),
		SwapMax:    limit(filepath.Join(path, "memory.swap.max")),
	}
	stat := keyed(filepath.Join(path, "memory.stat"))
	s.AnonMiB = int(stat["anon"] >> 20)
	ev := keyed(filepath.Join(path, "memory.events"))
	s.OOMKills, s.HighEvents = ev["oom_kill"], ev["high"]
	return s
}

func readInt(path string) int64 {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	return n
}

// limit renders a cgroup limit file in MiB, or "max".
func limit(path string) string {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "?"
	}
	v := strings.TrimSpace(string(raw))
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return strconv.FormatInt(n>>20, 10)
	}
	return v
}

func keyed(path string) map[string]int64 {
	out := map[string]int64{}
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return out
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if k, v, ok := strings.Cut(line, " "); ok {
			out[k], _ = strconv.ParseInt(v, 10, 64)
		}
	}
	return out
}

// Slot is one of memcap's build slots.
type Slot struct {
	N      int    `json:"n"`
	Free   bool   `json:"free"`
	Holder string `json:"holder,omitempty"`
}

// ReadSlots probes memcap's slot locks without blocking.
func ReadSlots(dir string, n int) []Slot {
	out := make([]Slot, 0, n)
	for i := 1; i <= n; i++ {
		s := Slot{N: i}
		lock := filepath.Join(dir, strconv.Itoa(i)+".lock")
		if _, err := os.Stat(lock); err != nil {
			s.Free = true
			out = append(out, s)
			continue
		}
		l := flock.New(lock)
		if ok, err := l.TryLock(); err == nil && ok {
			_ = l.Unlock()
			s.Free = true
		} else if raw, err := os.ReadFile(filepath.Clean(filepath.Join(dir, strconv.Itoa(i)+".holder"))); err == nil {
			s.Holder = strings.TrimSpace(string(raw))
		}
		out = append(out, s)
	}
	return out
}

// Cluster is a running kind cluster.
type Cluster struct {
	Name   string `json:"name"`
	Nodes  int    `json:"nodes"`
	MemMiB int    `json:"memMiB"`
	// Containers are the node containers' full ids.
	Containers []string `json:"containers"`
	// RunningFor is docker's age of its oldest node ("2 hours ago").
	RunningFor string `json:"runningFor"`
}

// KindClusters lists the kind clusters from the node containers docker runs.
func KindClusters(ctx context.Context) ([]Cluster, error) {
	out, err := exec.CommandContext(ctx, "docker", "ps", "--no-trunc", "--filter", "label=io.x-k8s.kind.cluster",
		"--format", `{{.ID}}\t{{.Label "io.x-k8s.kind.cluster"}}\t{{.RunningFor}}`).Output()
	if err != nil {
		return nil, err
	}
	by := map[string]*Cluster{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			continue
		}
		id, name := f[0], f[1]
		c := by[name]
		if c == nil {
			// docker lists the newest container first: the last node seen
			// is the oldest.
			c = &Cluster{Name: name}
			by[name] = c
		}
		c.RunningFor = f[2]
		c.Nodes++
		c.Containers = append(c.Containers, id)
		c.MemMiB += int(readInt("/sys/fs/cgroup/system.slice/docker-"+id+".scope/memory.current") >> 20)
	}
	var cs []Cluster
	for _, c := range by {
		cs = append(cs, *c)
	}
	slices.SortFunc(cs, func(a, b Cluster) int { return strings.Compare(a.Name, b.Name) })
	return cs, nil
}

// OOMKill is one process the kernel killed for memory.
type OOMKill struct {
	At         time.Time `json:"at"`
	PID        int       `json:"pid"`
	Task       string    `json:"task"`
	AnonMiB    int       `json:"anonMiB"`
	Constraint string    `json:"constraint,omitempty"`
	// Memcg is the cgroup whose limit was hit: a memcap scope, a kind
	// lab's pod, or the desktop scope.
	Memcg string `json:"memcg,omitempty"`
}

var (
	oomLine    = regexp.MustCompile(`oom-kill:constraint=([A-Z_]+),.*?oom_memcg=([^,]*),.*?task=([^,]*),pid=(\d+)`)
	killedLine = regexp.MustCompile(`Killed process (\d+) \(([^)]*)\).*?anon-rss:(\d+)kB`)
)

// OOMKills returns the kernel's OOM kills since the given time, oldest first.
func OOMKills(ctx context.Context, since time.Time) ([]OOMKill, error) {
	out, err := exec.CommandContext(ctx, "journalctl", "-k", "--no-pager", "-o", "short-iso", //nolint:gosec // fixed arguments and a formatted time
		"--since", since.Local().Format("2006-01-02 15:04:05")).Output()
	if err != nil {
		return nil, err
	}
	return ParseOOM(string(out)), nil
}

// ParseOOM pairs the kernel's oom-kill and "Killed process" lines by pid.
func ParseOOM(journal string) []OOMKill {
	var kills []OOMKill
	idx := map[int]int{}
	for line := range strings.SplitSeq(journal, "\n") {
		at := journalTime(line)
		if m := oomLine.FindStringSubmatch(line); m != nil {
			pid, _ := strconv.Atoi(m[4])
			idx[pid] = len(kills)
			kills = append(kills, OOMKill{At: at, PID: pid, Task: m[3], Constraint: m[1], Memcg: m[2]})
			continue
		}
		if m := killedLine.FindStringSubmatch(line); m != nil {
			pid, _ := strconv.Atoi(m[1])
			kb, _ := strconv.Atoi(m[3])
			i, ok := idx[pid]
			if !ok {
				idx[pid] = len(kills)
				kills = append(kills, OOMKill{At: at, PID: pid, Task: m[2]})
				i = len(kills) - 1
			}
			kills[i].AnonMiB = kb / 1024
		}
	}
	return kills
}

func journalTime(line string) time.Time {
	f, _, _ := strings.Cut(line, " ")
	t, err := time.Parse("2006-01-02T15:04:05-0700", f)
	if err != nil {
		t, _ = time.Parse(time.RFC3339, f)
	}
	return t
}

// OomdKills returns the lines where systemd-oomd killed a cgroup since the
// given time: the event that takes every session down at once.
func OomdKills(ctx context.Context, since time.Time) ([]string, error) {
	out, err := exec.CommandContext(ctx, "journalctl", "-u", "systemd-oomd", "--no-pager", "-o", "short-iso", //nolint:gosec // fixed arguments and a formatted time
		"--since", since.Local().Format("2006-01-02 15:04:05")).Output()
	if err != nil {
		return nil, err
	}
	var lines []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.Contains(strings.ToLower(line), "killed") {
			lines = append(lines, line)
		}
	}
	return lines, nil
}
