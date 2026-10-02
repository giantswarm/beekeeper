package machine

import (
	"strings"
	"testing"
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
