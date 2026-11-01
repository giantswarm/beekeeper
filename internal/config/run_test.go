package config

import (
	"testing"
	"time"
)

func TestRemoteRunConfig(t *testing.T) {
	raw := []byte(`run:
  remote:
    enabled: true
    kubeconfig: /config/build
    context: build-test
    namespace: builders
    images: {go: "builder:test"}
    cpu: "1500m"
    memory: 3Gi
    maxPods: 4
    placementTimeout: 45s
    idleTimeout: 2h
`)
	c, err := parse("/config/test.yaml", raw)
	if err != nil {
		t.Fatal(err)
	}
	r := c.Run.Remote
	if !r.Enabled || r.MaxPods != 4 || r.PlacementTimeout != 45*time.Second || r.IdleTimeout != 2*time.Hour || r.Images["go"] != "builder:test" {
		t.Fatalf("remote config not loaded: %+v", r)
	}
	if _, err := parse("/config/test.yaml", []byte("run:\n  remote:\n    enabled: true\n")); err == nil {
		t.Fatal("enabled remote run without cluster accepted")
	}
}
