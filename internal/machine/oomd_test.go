package machine

import (
	"slices"
	"strings"
	"testing"
)

const dumpNoSwap = `Dry Run: no
Swap Used Limit: 90.00%
Default Memory Pressure Limit: 60.00%
Default Memory Pressure Duration: 30s
System Context:
	Memory: Used: 41.8G, Total: 86G
	Swap: Used: 8.1G, Total: 15.9G
Swap Monitored CGroups:
Memory Pressure Monitored CGroups:
	Path: /user.slice/user-1000.slice/user@1000.service
		Memory Pressure Limit: 0.00%
		Pressure: Avg10: 0.00 Avg60: 0.00 Avg300: 0.00 Total: 2s
`

const dumpSwap = `Dry Run: no
Swap Used Limit: 80.00%
Default Memory Pressure Limit: 60.00%
System Context:
	Swap: Used: 8.1G, Total: 15.9G
Swap Monitored CGroups:
	Path: /user.slice/user-1000.slice/user@1000.service/app.slice
		Swap Usage: 3.1G
	Path: /system.slice
		Swap Usage: 0B
Memory Pressure Monitored CGroups:
	Path: /user.slice
`

func TestParseOomctlDump(t *testing.T) {
	m := Mem{SwapTotalMiB: 16383, SwapUsedMiB: 10627}
	o, err := ParseOomctlDump(dumpNoSwap)
	if err != nil || o.LimitPercent != 90 || o.Watched() {
		t.Fatalf("no swap rule: %+v %v", o, err)
	}
	if l := o.Line(m); strings.Contains(l, "%") || l != "systemd-oomd watches no cgroup for swap" {
		t.Errorf("unwatched line %q", l)
	}
	o, err = ParseOomctlDump(dumpSwap)
	want := []string{"/user.slice/user-1000.slice/user@1000.service/app.slice", "/system.slice"}
	if err != nil || o.LimitPercent != 80 || !slices.Equal(o.Monitored, want) {
		t.Fatalf("swap rule: %+v %v", o, err)
	}
	if l := o.Line(m); l != "2479 MiB before systemd-oomd's 80 % swap trigger (2 swap-monitored cgroups)" {
		t.Errorf("watched line %q", l)
	}
	if _, err := ParseOomctlDump("Dry Run: no\n"); err == nil {
		t.Error("a dump without a limit parsed")
	}
}

func TestParsePercent(t *testing.T) {
	for v, want := range map[string]int{"90.00%": 90, "955‰": 95, "9000‱": 90} {
		if got, ok := parsePercent(v); !ok || got != want {
			t.Errorf("%q: %d %v", v, got, ok)
		}
	}
	if _, ok := parsePercent("90"); ok {
		t.Error("a bare number parsed")
	}
}

func TestOOMDHeadroom(t *testing.T) {
	if got := (OOMDSwap{LimitPercent: 90}).HeadroomMiB(Mem{SwapTotalMiB: 16383, SwapUsedMiB: 10627}); got != 4117 {
		t.Errorf("headroom %d", got)
	}
}
