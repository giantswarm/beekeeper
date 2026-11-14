package config

import "testing"

func TestGuardsExplicitOptOut(t *testing.T) {
	if !(Hooks{}).GuardsEnabled() {
		t.Fatal("unset must enforce guards")
	}
	for _, enabled := range []bool{true, false} {
		if (Hooks{Guards: &enabled}).GuardsEnabled() != enabled {
			t.Fatal("explicit switch ignored")
		}
	}
}

func TestDiskFloorOff(t *testing.T) {
	for _, tc := range []struct{ total, set, want int }{
		{100000, -1, 0},
		{100000, 0, 5000},
		{100000, 123, 5000},
		{300000, -1, 0},
		{300000, 0, 15000},
		{300000, 1, 10240},
		{300000, 2048, 10240},
		{300000, 20480, 20480},
	} {
		if got := (Watch{DiskMinMiB: tc.set}).DiskMin(tc.total); got != tc.want {
			t.Fatalf("total %d set %d: got %d want %d", tc.total, tc.set, got, tc.want)
		}
	}
}
