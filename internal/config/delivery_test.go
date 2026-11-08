package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeliveryLimitConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	for _, tc := range []struct {
		raw  string
		want int
		bad  bool
	}{{"", 0, false}, {"delivery: {wipLimit: 0}\n", 0, false}, {"delivery: {wipLimit: 8}\n", 8, false}, {"delivery: {wipLimit: -1}\n", 0, true}} {
		if err := os.WriteFile(p, []byte(tc.raw), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(p)
		if (err != nil) != tc.bad {
			t.Fatalf("%q: %v", tc.raw, err)
		}
		if err == nil && c.Delivery.WIPLimit != tc.want {
			t.Fatal(c.Delivery)
		}
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := Set(p, []Setting{{Key: "delivery.wipLimit", Value: "12"}}); err != nil || c.Delivery.WIPLimit != 12 {
		t.Fatalf("set: %+v %v", c, err)
	}
	if _, err := Set(p, []Setting{{Key: "delivery.wipLimit", Value: "-3"}}); err == nil {
		t.Fatal("negative set accepted")
	}
	c, err := Load(p)
	if err != nil || c.Delivery.WIPLimit != 12 {
		t.Fatalf("failed set changed config: %+v %v", c, err)
	}
}
func TestDeliveryExampleConfig(t *testing.T) {
	c, err := Load("../../docs/examples/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Delivery.WIPLimit != 0 {
		t.Fatal(c.Delivery)
	}
	// The complete example names optional desk input files. Supply placeholders
	// in an isolated copy so config check exercises the full example safely.
	dir := t.TempDir()
	raw, err := os.ReadFile("../../docs/examples/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	example := filepath.Join(dir, "config.yaml")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile("config.yaml", []byte(strings.ReplaceAll(string(raw), "~/", dir+"/")), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".kube/config", ".local/state/beekeeper/kube/teleport.yaml", ".local/state/desk/report-brief.md", ".omp/agent/models.yml"} {
		file := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if findings, err := Check(example); err != nil || len(findings) != 0 {
		t.Fatalf("example config check: %v %v", findings, err)
	}
}

func TestDeliveryStallAfter(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	for _, tc := range []struct {
		raw  string
		want time.Duration
		bad  bool
	}{
		{"", 24 * time.Hour, false},
		{"delivery: {stallAfter: 2h}", 2 * time.Hour, false},
		{"delivery: {stallAfter: -1h}", 0, true},
		{"delivery: {stallAfter: nonsense}", 0, true},
	} {
		if err := os.WriteFile(p, []byte(tc.raw), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(p)
		if (err != nil) != tc.bad {
			t.Fatalf("%s: %v", tc.raw, err)
		}
		if err == nil && c.Delivery.StallAfter.Duration != tc.want {
			t.Fatal(c.Delivery)
		}
	}
}

func TestDeliveryExemptBoardStepConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("board:\n  order:\n    - name: emergency\n      labels: [page]\n      wipExempt: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Board.Order) != 1 || !c.Board.Order[0].WIPExempt {
		t.Fatal(c.Board.Order)
	}
}
