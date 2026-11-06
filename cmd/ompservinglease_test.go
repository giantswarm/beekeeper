package cmd

import (
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
)

const ompServingLane = "serving"

func TestOmpServingLease(t *testing.T) {
	a := &app{cfg: &config.Config{}}
	a.cfg.LeaseDir = t.TempDir()
	a.cfg.Omp.ServedBy = ompServingLane
	d := lease.Dir(a.cfg.LeaseDir)
	if err := a.ompServingLease(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Claim(ompServingLane, lease.Holder{Holder: "benchmark", Purpose: "test another model"}); err != nil {
		t.Fatal(err)
	}
	err := a.ompServingLease()
	if err == nil {
		t.Fatal("held resource admitted")
	}
	for _, want := range []string{ompServingLane, "benchmark", "test another model", "retry after lease release"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q: %v", want, err)
		}
	}
	if err := d.Release(ompServingLane); err != nil {
		t.Fatal(err)
	}
	if err := a.ompServingLease(); err != nil {
		t.Fatal(err)
	}
}
