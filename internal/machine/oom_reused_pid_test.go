package machine

import (
	"testing"
	"time"
)

func TestParseOOMReusedPID(t *testing.T) {
	const summary = "2026-11-01T12:00:00+0000 host kernel: oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),cpuset=/,mems_allowed=0,oom_memcg=/old.scope,task_memcg=/old.scope,task=old-worker,pid=42,uid=1000\n"
	const oldDetail = "2026-11-01T12:00:00+0000 host kernel: Memory cgroup out of memory: Killed process 42 (old-worker) total-vm:8192kB, anon-rss:2048kB, file-rss:0kB\n"
	const newDetail = "2026-11-01T12:30:00+0000 host kernel: Memory cgroup out of memory: Killed process 42 (new-worker) total-vm:16384kB, anon-rss:4096kB, file-rss:0kB"
	for _, tc := range []struct {
		name    string
		journal string
		memcg   string
	}{
		{name: "case 0: reuse after a paired victim", journal: summary + oldDetail + newDetail, memcg: "/old.scope"},
		{name: "case 1: reuse after a standalone detail", journal: oldDetail + newDetail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kills := ParseOOM(tc.journal)
			if len(kills) != 2 {
				t.Fatalf("kills = %+v, want two victims despite PID reuse", kills)
			}
			old, later := kills[0], kills[1]
			if old.PID != 42 || old.Task != "old-worker" || old.AnonMiB != 2 || old.Memcg != tc.memcg ||
				!old.At.Equal(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)) {
				t.Errorf("earlier victim changed: %+v", old)
			}
			if later.PID != 42 || later.Task != "new-worker" || later.AnonMiB != 4 || later.Memcg != "" || later.TaskMemcg != "" || later.Constraint != "" ||
				!later.At.Equal(time.Date(2026, 11, 1, 12, 30, 0, 0, time.UTC)) {
				t.Errorf("later victim lost its identity: %+v", later)
			}
		})
	}
}

func TestParseOOMInterleavedVictims(t *testing.T) {
	const journal = `2026-11-01T12:00:00+0000 host kernel: oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),cpuset=/,mems_allowed=0,oom_memcg=/first.scope,task_memcg=/first.scope,task=first,pid=41,uid=1000
2026-11-01T12:00:01+0000 host kernel: oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),cpuset=/,mems_allowed=0,oom_memcg=/second.scope,task_memcg=/second.scope,task=second,pid=42,uid=1000
2026-11-01T12:00:01+0000 host kernel: Memory cgroup out of memory: Killed process 42 (second) total-vm:8192kB, anon-rss:4096kB, file-rss:0kB
2026-11-01T12:00:02+0000 host kernel: Memory cgroup out of memory: Killed process 41 (first) total-vm:8192kB, anon-rss:2048kB, file-rss:0kB`
	kills := ParseOOM(journal)
	if len(kills) != 2 || kills[0].PID != 41 || kills[0].Memcg != "/first.scope" || kills[0].AnonMiB != 2 ||
		kills[1].PID != 42 || kills[1].Memcg != "/second.scope" || kills[1].AnonMiB != 4 {
		t.Errorf("interleaved kills = %+v, want details paired by PID", kills)
	}
}
