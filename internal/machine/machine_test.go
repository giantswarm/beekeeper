package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseOOM(t *testing.T) {
	journal := `2026-09-24T19:58:04+0300 demiurg kernel: node invoked oom-killer: gfp_mask=0xcc0(GFP_KERNEL), order=0, oom_score_adj=200
2026-09-24T19:58:04+0300 demiurg kernel: oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),cpuset=user.slice,mems_allowed=0,oom_memcg=/user.slice/user-1000.slice/user@1000.service/memcap.slice/memcap-1370955-031172.scope,task_memcg=/user.slice/user-1000.slice/user@1000.service/memcap.slice/memcap-1370955-031172.scope,task=MainThread,pid=1376734,uid=1000
2026-09-24T19:58:04+0300 demiurg kernel: Memory cgroup out of memory: Killed process 1376734 (MainThread) total-vm:4484504kB, anon-rss:962572kB, file-rss:87508kB, shmem-rss:0kB, UID:1000 pgtables:22248kB oom_score_adj:200
2026-09-24T20:01:00+0300 demiurg kernel: Out of memory: Killed process 42 (jest worker) total-vm:1kB, anon-rss:2048kB, file-rss:0kB`
	kills := ParseOOM(journal)
	if len(kills) != 2 {
		t.Fatalf("kills = %+v", kills)
	}
	k := kills[0]
	if k.PID != 1376734 || k.Task != "MainThread" || k.AnonMiB != 940 || k.Constraint != "CONSTRAINT_MEMCG" ||
		k.Memcg != "/user.slice/user-1000.slice/user@1000.service/memcap.slice/memcap-1370955-031172.scope" || k.At.IsZero() {
		t.Errorf("first kill = %+v", k)
	}
	if kills[1].PID != 42 || kills[1].Task != "jest worker" || kills[1].AnonMiB != 2 {
		t.Errorf("second kill = %+v", kills[1])
	}
}

// memcap.slice's CPU: cpu.max as a systemd quota, cpu.weight, and the use
// and the throttled periods over the sample from cpu.stat before and after.
func TestReadBuildCPU(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("cpu.max", "1200000 100000\n")
	write("cpu.weight", "50\n")
	write("cpu.stat", "usage_usec 1000000\nuser_usec 900000\nsystem_usec 100000\nnr_periods 10\nnr_throttled 2\nthrottled_usec 5000\n")
	var slept time.Duration
	later := func(d time.Duration) {
		slept = d
		write("cpu.stat", "usage_usec 3400000\nuser_usec 3000000\nsystem_usec 400000\nnr_periods 12\nnr_throttled 5\nthrottled_usec 9000\n")
	}
	b := readBuildCPU(dir, 200*time.Millisecond, later)
	if slept != 200*time.Millisecond || b.Quota != "1200%" || b.Weight != 50 || b.UsePct != 1200 || b.Throttled != 3 {
		t.Errorf("readBuildCPU = %+v after %s", b, slept)
	}

	write("cpu.max", "max 100000\n")
	if b := readBuildCPU(dir, 0, func(time.Duration) {}); b.Quota != "max" || b.UsePct != 0 || b.Throttled != 0 {
		t.Errorf("without a quota or a sample: %+v", b)
	}
	for file, want := range map[string]string{"150000 100000\n": "150%", "50000 100000": "50%", "garbage": "?", "1 0": "?"} {
		write("cpu.max", file)
		if got := cpuQuota(filepath.Join(dir, "cpu.max")); got != want {
			t.Errorf("cpuQuota(%q) = %q, want %q", file, got, want)
		}
	}
	if got := cpuQuota(filepath.Join(dir, "missing")); got != "?" {
		t.Errorf("cpuQuota of a missing file = %q", got)
	}
}

func TestParseMem(t *testing.T) {
	const base = "MemTotal:       90177536 kB\nMemAvailable:   41943040 kB\nShmem:           1048576 kB\nSwapTotal:      16777212 kB\nSwapFree:        8222972 kB\n"
	m, err := parseMem(strings.NewReader(base + "Zswap:           1719720 kB\nZswapped:        3177092 kB\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.SwapUsedMiB != 8353 || m.ZswappedMiB != 3102 || m.ZswapPoolMiB != 1679 || m.DiskSwapMiB() != 5251 {
		t.Fatalf("zswap: %+v disk %d", m, m.DiskSwapMiB())
	}
	if m.DiskSwapMiB()+m.ZswappedMiB != m.SwapUsedMiB {
		t.Errorf("disk %d + zswap %d is not the %d in use", m.DiskSwapMiB(), m.ZswappedMiB, m.SwapUsedMiB)
	}
	if got, want := m.SwapSplit(), "disk 5251 MiB + zswap 3102 MiB in a 1679 MiB pool"; got != want {
		t.Errorf("split %q, want %q", got, want)
	}
	m, _ = parseMem(strings.NewReader(base))
	if m.ZswappedMiB != 0 || m.DiskSwapMiB() != m.SwapUsedMiB {
		t.Errorf("no zswap: %+v", m)
	}
	if (Mem{SwapUsedMiB: 100, ZswappedMiB: 120}).DiskSwapMiB() != 0 {
		t.Error("disk swap below zero")
	}
}

func TestParseOOMAtASlotSlice(t *testing.T) {
	const slice = "/user.slice/user-1000.slice/user@1000.service/memcap.slice/memcap-slot2.slice"
	journal := `2026-10-04T16:00:00+0300 demiurg kernel: oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),cpuset=user.slice,mems_allowed=0,oom_memcg=` + slice + `,task_memcg=` + slice + `/memcap-42-000001.scope,task=node,pid=43,uid=1000`
	kills := ParseOOM(journal)
	if len(kills) != 1 || kills[0].Memcg != slice || kills[0].RunMemcg() != slice+"/memcap-42-000001.scope" || kills[0].Task != "node" || kills[0].PID != 43 {
		t.Errorf("kills = %+v", kills)
	}
}
