package config

import (
	"slices"
	"testing"
)

func TestFatalConfigDefaultsOptOutAndValidation(t *testing.T) {
	c, err := parse("config.yaml", nil)
	if err != nil || !slices.Equal(c.Fatal.Repositories, []string{"giantswarm/beekeeper"}) {
		t.Fatal(c, err)
	}
	c, err = parse("config.yaml", []byte("fatal: {repositories: []}"))
	if err != nil || len(c.Fatal.Repositories) != 0 {
		t.Fatal(c, err)
	}
	for _, raw := range []string{
		"fatal: {checks: [{name: a, probe: []}]}",
		"fatal: {checks: [{name: a, probe: [true]}, {name: a, probe: [true]}]}",
		"fatal: {checks: [{name: a, probe: [true], harness: unknown}]}",
		"fatal: {repositories: [bad]}",
	} {
		if _, err := parse("config.yaml", []byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
