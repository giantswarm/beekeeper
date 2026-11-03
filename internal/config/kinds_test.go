package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAgentKinds(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	raw := `agents:
  kinds:
    - {name: filer, match: '^filer', maxBusy: 1, minInterval: 1h, dailySpend: 20}
    - {name: meta, match: '^(filer|scan|closer)', maxBusy: 2}
`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	ks := c.Agents.KindsOf("filer 3")
	if len(ks) != 2 || ks[0].Name != "filer" || ks[0].MaxBusy != 1 || ks[0].MinInterval.Duration != time.Hour || ks[0].DailySpend != 20 || ks[1].Name != "meta" {
		t.Fatalf("KindsOf(filer 3) = %+v", ks)
	}
	if ks := c.Agents.KindsOf("scan board"); len(ks) != 1 || ks[0].Name != "meta" {
		t.Fatalf("KindsOf(scan board) = %+v", ks)
	}
	if ks := c.Agents.KindsOf("beekeeper 985"); len(ks) != 0 {
		t.Fatalf("KindsOf(beekeeper 985) = %+v", ks)
	}
	for _, bad := range []string{
		`agents: {kinds: [{name: filer, match: '['}]}`,
		`agents: {kinds: [{name: filer}]}`,
		`agents: {kinds: [{match: '^filer'}]}`,
		`agents: {kinds: [{name: 'a b', match: x}]}`,
		`agents: {kinds: [{name: f, match: x}, {name: f, match: y}]}`,
		`agents: {kinds: [{name: f, match: x, maxBusy: -1}]}`,
		`agents: {kinds: [{name: f, match: x, minInterval: -1h}]}`,
		`agents: {kinds: [{name: f, match: x, dailySpend: -1}]}`,
	} {
		if err := os.WriteFile(p, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
