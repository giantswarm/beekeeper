package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
)

// The base ignores the new disk reading and incorrectly allows a start.
// Only pre-existing helpers are used so this regression runs on the base.
func TestCapacityLowDiskReadingHoldsStarts(t *testing.T) {
	h := roomy()
	if err := json.Unmarshal([]byte(`{"disk":{"Low":true,"FreeMiB":1,"MinMiB":100}}`), h); err != nil {
		t.Fatal(err)
	}
	k := config.Capacity{Floor: 1, Ceiling: 3}
	for _, builds := range []bool{false, true} {
		v := newCapacity(agentCount{}, k, h, builds)
		if v.Room != 0 || !strings.Contains(v.verdict(), "disk pressure") {
			t.Fatalf("low disk allowed a start: %s", v.verdict())
		}
	}
	if !strings.Contains(h.line(), "free disk / 1 MiB (floor 100 MiB)") {
		t.Fatalf("disk absent from headroom: %s", h.line())
	}
	if err := json.Unmarshal([]byte(`{"disk":{"Low":false,"FreeMiB":200,"MinMiB":100}}`), h); err != nil {
		t.Fatal(err)
	}
	if v := newCapacity(agentCount{}, k, h, false); v.Room == 0 {
		t.Fatalf("disk recovery did not release starts: %s", v.verdict())
	}
}
