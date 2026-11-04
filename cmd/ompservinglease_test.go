package cmd

import (
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
)

func TestOmpServingLease(t *testing.T) {
	a := &app{cfg: &config.Config{}}
	a.cfg.LeaseDir = t.TempDir()
	a.cfg.Omp.ServedBy = "serving"
	d := lease.Dir(a.cfg.LeaseDir)
	if err := a.ompServingLease(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Claim("serving", lease.Holder{Holder: "benchmark", Purpose: "test another model"}); err != nil {
		t.Fatal(err)
	}
	err := a.ompServingLease()
	if err == nil {
		t.Fatal("held resource admitted")
	}
	for _, want := range []string{"serving", "benchmark", "test another model", "retry after lease release"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q: %v", want, err)
		}
	}
	if err := d.Release("serving"); err != nil {
		t.Fatal(err)
	}
	if err := a.ompServingLease(); err != nil {
		t.Fatal(err)
	}
}
