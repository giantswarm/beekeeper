package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReadOnlyExecQueriesConfig(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("kube:\n  readOnlyExecQueries:\n    - select * from conversations limit 5\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Kube.ReadOnlyExecQueries) != 1 || cfg.Kube.ReadOnlyExecQueries[0] != "select * from conversations limit 5" {
		t.Fatalf("query allowlist not decoded: %v", cfg.Kube.ReadOnlyExecQueries)
	}
}
