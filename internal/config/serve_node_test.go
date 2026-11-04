package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const serveNodeConfig = `capacity:
  providers:
    fixture:
      kind: self-hosted
      serveModeCommand: [offline-read-mode]
      serveModeSwitchCommand: [offline-switch-mode]
      serveModeModes:
        full: {nodes: [%q], sessions: 1}
`

func TestServeModeNodeResourceNames(t *testing.T) {
	for _, node := range []string{"../outside", "nested/node", "/absolute", string(filepath.Separator), ".", "..", ".hidden", "", "  ", Browser, ModelServer} {
		t.Run(fmt.Sprintf("%q", node), func(t *testing.T) {
			p := writeConfig(t, fmt.Sprintf(serveNodeConfig, node))
			if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "serveModeModes.full") {
				t.Errorf("unsafe node resource %q loaded: %v", node, err)
			}
			if findings, err := Check(p); err != nil || len(findings) != 1 || !strings.Contains(findings[0], "serveModeModes.full") {
				t.Errorf("unsafe node resource %q checked: %v, %v", node, findings, err)
			}
		})
	}
	p := writeConfig(t, "resources: [node-0]\n"+fmt.Sprintf(serveNodeConfig, "node-0"))
	c, err := Load(p)
	if err != nil || !c.IsLeasable("node-0") {
		t.Fatalf("valid shared resource must load and be leasable: %v", err)
	}
}

func TestLeaseResourceRejectsRoot(t *testing.T) {
	p := writeConfig(t, fmt.Sprintf("resources: [%q]\n", string(filepath.Separator)))
	if _, err := Load(p); err == nil {
		t.Fatal("filesystem root accepted as a lease resource")
	}
}

func TestSetRejectsUnsafeServeModeNode(t *testing.T) {
	raw := fmt.Sprintf(serveNodeConfig, "node-0")
	p := writeConfig(t, raw)
	_, err := Set(p, []Setting{{Key: "capacity.providers.fixture.serveModeModes.full.nodes", Value: "[../outside]"}})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("unsafe node edit must be refused: %v", err)
	}
	if got, err := os.ReadFile(filepath.Clean(p)); err != nil || string(got) != raw {
		t.Fatalf("refused edit changed config: %v", err)
	}
}
