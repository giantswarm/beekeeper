package cmd

import "testing"

func TestGoconstProbe(t *testing.T) {
	a, b, c := "goconst-probe", "goconst-probe", "goconst-probe"
	if a != b || b != c {
		t.Fatal("unreachable")
	}
}
