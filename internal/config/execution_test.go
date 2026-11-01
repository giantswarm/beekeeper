package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSupervisorHostsValidateAgainstTheCatalog(t *testing.T) {
	const entry = `    %s:
      identity: %s
      ssh: host-a.example
      executable: /usr/local/bin/beekeeper
      config: /etc/beekeeper/host.yaml
      workspaceRoot: /srv/workspaces
      repositories: {example/repository: /srv/source}
      harnesses: [claude]
      policyDigest: %s
      maxBusy: 2
`
	digest := "sha256:" + strings.Repeat("a", 64)
	host := fmt.Sprintf(entry, "host-a", "host-a", digest)
	for _, tc := range []struct{ name, raw, want string }{
		{"supervisor hosts omitted", "agents:\n  hosts:\n" + host, ""},
		{"known", "agents:\n  hosts:\n" + host + "supervisor:\n  hosts: [host-a]\n", ""},
		{"hostsFirst alone", "supervisor:\n  hostsFirst: true\n", ""},
		{"unknown", "supervisor:\n  hosts: [host-z]\n", "no agents.hosts entry"},
		{"local", "agents:\n  hosts:\n" + host + "supervisor:\n  hosts: [local]\n", "no SSH execution host"},
		{"empty host entry", "supervisor:\n  hosts: [\"\"]\n", "no SSH execution host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(p)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("validation: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "known" && !slices.Equal(cfg.Supervisor.Hosts, []string{"host-a"}) {
				t.Fatalf("hosts = %v", cfg.Supervisor.Hosts)
			}
		})
	}
}

func TestExecutionHostConfigValidatesCatalogWithoutChangingLocalDefaults(t *testing.T) {
	const entry = `    %s:
      identity: %s
      ssh: host-a.example
      executable: /usr/local/bin/beekeeper
      config: /etc/beekeeper/host.yaml
      workspaceRoot: /srv/workspaces
      repositories: {example/repository: /srv/source}
      harnesses: [claude]
      policyDigest: %s
      maxBusy: 2
`
	digest := "sha256:" + strings.Repeat("a", 64)
	first := fmt.Sprintf(entry, "host-a", "host-a", digest)
	for _, tc := range []struct{ name, raw, want string }{
		{"absent", "", ""},
		{"valid", "agents:\n  hosts:\n" + first, ""},
		{"reserved", "agents:\n  hosts:\n" + fmt.Sprintf(entry, "local", "host-a", digest), "local is reserved"},
		{"duplicate identity", "agents:\n  hosts:\n" + first + fmt.Sprintf(entry, "host-b", "host-a", digest), "unique identities"},
		{"unsafe endpoint", "agents:\n  hosts:\n" + strings.ReplaceAll(first, "host-a.example", "-unsafe"), "SSH endpoint"},
		{"unsupported harness", "agents:\n  hosts:\n" + strings.ReplaceAll(first, "[claude]", "[codex]"), "harnesses [claude]"},
		{"workspace root", "agents:\n  hosts:\n" + strings.ReplaceAll(first, "/srv/workspaces", "/"), "workspace root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(p)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("validation: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Agents.Mode != ModeDesktop || cfg.Agents.Execution != nil {
				t.Fatal("local defaults changed")
			}
			if tc.name == "valid" && cfg.Agents.Hosts["host-a"].SSH != "host-a.example" {
				t.Fatal("catalog missing")
			}
		})
	}
}
