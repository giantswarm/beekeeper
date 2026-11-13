package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/config"
)

// The base ignores the new disk reading and incorrectly allows a start.
// Only pre-existing helpers are used so this regression runs on the base.
func TestCapacityLowDiskReadingHoldsStarts(t *testing.T) {
	h := roomy()
	if err := json.Unmarshal([]byte(`{"disk":{"Low":true,"FreeMiB":1,"MinMiB":100}}`), h); err != nil {
		t.Fatal(err)
	}
	k := config.Capacity{Ceiling: 3}
	for _, builds := range []bool{false, true} {
		v := capacity.NewCapacity(capacity.AgentCount{}, k, h, builds)
		if v.Room != 0 || !strings.Contains(v.Verdict(), "disk pressure") {
			t.Fatalf("low disk allowed a start: %s", v.Verdict())
		}
	}
	if !strings.Contains(h.Line(), "free disk / 1 MiB (floor 100 MiB)") {
		t.Fatalf("disk absent from headroom: %s", h.Line())
	}
	if err := json.Unmarshal([]byte(`{"disk":{"Low":false,"FreeMiB":200,"MinMiB":100}}`), h); err != nil {
		t.Fatal(err)
	}
	if v := capacity.NewCapacity(capacity.AgentCount{}, k, h, false); v.Room == 0 {
		t.Fatalf("disk recovery did not release starts: %s", v.Verdict())
	}
}
