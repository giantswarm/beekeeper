package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func leaseResourceFixture(name string, implicit bool) (string, Setting, string) {
	if implicit {
		modes := fmt.Sprintf("{full: {nodes: [%q], sessions: 1}}", name)
		return "capacity:\n  providers:\n    fixture:\n      kind: self-hosted\n      serveModeCommand: [offline-read-mode]\n      serveModeSwitchCommand: [offline-switch-mode]\n      serveModeModes: " + modes + "\n",
			Setting{"capacity.providers.fixture.serveModeModes", modes}, "capacity.providers.fixture.serveModeModes.full"
	}
	value := fmt.Sprintf("[%q]", name)
	return "resources: " + value + "\n", Setting{"resources", value}, "resources:"
}

func TestLeaseResourceValidation(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		source := "resources"
		if implicit {
			source = "serving-mode nodes"
		}
		t.Run(source, func(t *testing.T) {
			for _, name := range []string{"", " ", "../outside", "nested/node", "/outside", string(filepath.Separator), ".", "..", ".hidden", Browser, ModelServer} {
				t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
					raw, setting, context := leaseResourceFixture(name, implicit)
					p := writeConfig(t, raw)
					if _, err := Load(p); err == nil || !strings.Contains(err.Error(), context) {
						t.Errorf("Load error = %v, want resource validation at %s", err, context)
					}
					findings, err := Check(p)
					if err != nil || !strings.Contains(strings.Join(findings, "\n"), context) {
						t.Errorf("Check = %v, %v; want finding at %s", findings, err, context)
					}
					valid, _, _ := leaseResourceFixture("fixture-node", implicit)
					p = writeConfig(t, valid)
					if _, err := Set(p, []Setting{setting}); !errors.Is(err, ErrInvalid) {
						t.Errorf("Set error = %v, want ErrInvalid", err)
					}
					after, err := os.ReadFile(p)
					if err != nil {
						t.Fatal(err)
					}
					if string(after) != valid {
						t.Error("Set changed the file after refusing an invalid resource")
					}
				})
			}
		})
	}
}

func TestLeaseResourceServingNodeAlsoExplicit(t *testing.T) {
	raw, setting, _ := leaseResourceFixture("fixture-node", true)
	raw = "resources: [fixture-node]\n" + raw
	p := writeConfig(t, raw)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsLeasable("fixture-node") {
		t.Error("valid serving node is not leasable")
	}
	count := 0
	for _, resource := range c.Leasable() {
		if resource == "fixture-node" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("node appears %d times in leasable resources", count)
	}
	if findings, err := Check(p); err != nil || len(findings) != 0 {
		t.Fatalf("Check = %v, %v", findings, err)
	}
	if _, err := Set(p, []Setting{setting}); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
}
