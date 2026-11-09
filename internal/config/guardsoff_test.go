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
	for _, tc := range []struct{ set, want int }{{-1, 0}, {0, 5000}, {123, 2048}} {
		if got := (Watch{DiskMinMiB: tc.set}).DiskMin(100000); got != tc.want {
			t.Fatalf("set %d: got %d want %d", tc.set, got, tc.want)
		}
	}
}
