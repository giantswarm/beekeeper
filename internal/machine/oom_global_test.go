package machine

import (
	"testing"
	"time"
)

func TestParseOOMGlobal(t *testing.T) {
	const summary = "2026-11-01T12:00:00+0000 host kernel: oom-kill:constraint=CONSTRAINT_NONE,nodemask=(null),cpuset=/,mems_allowed=0,global_oom,task_memcg=/user.slice/example.scope,task=worker,pid=42,uid=1000"
	const detail = "2026-11-01T12:00:00+0000 host kernel: Out of memory: Killed process 42 (worker) total-vm:8192kB, anon-rss:2048kB, file-rss:0kB, shmem-rss:0kB"
	for _, tc := range []struct {
		name    string
		journal string
		anonMiB int
	}{
		{name: "case 0: summary before detail arrives", journal: summary},
		{name: "case 1: summary paired with detail", journal: summary + "\n" + detail, anonMiB: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kills := ParseOOM(tc.journal)
			if len(kills) != 1 {
				t.Fatalf("kills = %+v, want one global OOM victim", kills)
			}
			want := OOMKill{
				At:         time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
				PID:        42,
				Task:       "worker",
				AnonMiB:    tc.anonMiB,
				Constraint: "CONSTRAINT_NONE",
				TaskMemcg:  "/user.slice/example.scope",
			}
			got := kills[0]
			if !got.At.Equal(want.At) || got.PID != want.PID || got.Task != want.Task || got.AnonMiB != want.AnonMiB ||
				got.Constraint != want.Constraint || got.Memcg != "" || got.TaskMemcg != want.TaskMemcg || got.RunMemcg() != want.TaskMemcg {
				t.Errorf("kill = %+v, want %+v", got, want)
			}
		})
	}
}
