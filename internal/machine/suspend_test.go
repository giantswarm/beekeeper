package machine

import (
	"testing"
	"time"
)

func TestParseSuspend(t *testing.T) {
	records := ParseSuspend(`unrelated PM: suspend entry
2026-10-01T10:00:00.123456+0000 kernel: PM: suspend entry (deep)
2026-10-01T10:30:00.654321+0000 kernel: unrelated
2026-10-01T12:00:00.123456+0000 kernel: PM: suspend exit`)
	if len(records) != 2 || !records[0].Entry || records[1].Entry || records[1].At.Sub(records[0].At) != 2*time.Hour {
		t.Fatalf("records: %+v", records)
	}
}
