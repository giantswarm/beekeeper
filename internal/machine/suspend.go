package machine

import (
	"strings"
	"time"
)

// SuspendRecord is a kernel power-management boundary, without journal metadata.
type SuspendRecord struct {
	At    time.Time
	Entry bool
}

// ParseSuspend keeps only timestamped suspend boundaries. Pairing is left to
// the caller because entry and exit may arrive in different polls.
func ParseSuspend(journal string) []SuspendRecord {
	var records []SuspendRecord
	for line := range strings.SplitSeq(journal, "\n") {
		entry := strings.Contains(line, "PM: suspend entry")
		if !entry && !strings.Contains(line, "PM: suspend exit") {
			continue
		}
		if at := journalTime(line); !at.IsZero() {
			records = append(records, SuspendRecord{At: at, Entry: entry})
		}
	}
	return records
}
