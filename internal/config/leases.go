package config

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// Browser is the resource every machine has: the one Chrome the Claude in
// Chrome extension drives.
const Browser = "browser"

// ModelServer is the resource of the host's model server: a claim carries
// the GiB its models may hold.
const ModelServer = "model-server"

// validResourceName requires one local filename component. Leading dots and
// built-in lease names are reserved; serving-mode nodes share this contract
// because they become lease resources even when resources does not list them.
func validResourceName(name string) bool {
	return strings.TrimSpace(name) != "" && filepath.IsLocal(name) &&
		filepath.Base(name) == name && name[0] != '.' &&
		name != Browser && name != ModelServer
}

// Leasable returns every resource a session can lease: the configured ones,
// the installations under alerts, the nodes a self-hosted serving mode
// occupies, the model server when one is watched, the browser last.
func (c *Config) Leasable() []string {
	out := slices.Clone(c.Resources)
	for _, in := range c.Alerts.Installations {
		if !slices.Contains(out, in.Name) {
			out = append(out, in.Name)
		}
	}
	for _, n := range c.Capacity.serveModeNodeNames() {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	if c.Ollama.URL != "" || c.Lemonade.URL != "" {
		out = append(out, ModelServer)
	}
	return append(out, Browser)
}

// validateLabsEach checks that every lab lease is a configured resource and
// that no two lab leases map one kind cluster.
func (c *Config) validateLabsEach(report func(error)) {
	by := map[string]string{}
	for _, res := range slices.Sorted(maps.Keys(c.Labs)) {
		cl := c.Labs[res]
		if !slices.Contains(c.Resources, res) {
			report(fmt.Errorf("labs: %q is not one of the resources %s", res, strings.Join(c.Resources, ", ")))
		}
		if cl == "" {
			report(fmt.Errorf("labs: %q names no kind cluster", res))
		}
		if other, ok := by[cl]; ok {
			report(fmt.Errorf("labs: %q and %q both map the kind cluster %q", other, res, cl))
		}
		by[cl] = res
	}
}

// LabCluster is the kind cluster the lab lease res stands for, "" when res
// is no lab lease.
func (c *Config) LabCluster(res string) string { return c.Labs[res] }

// LabLease is the lab lease that stands for the kind cluster cluster, ""
// when none maps it.
func (c *Config) LabLease(cluster string) string {
	for res, cl := range c.Labs {
		if cl == cluster {
			return res
		}
	}
	return ""
}

// IsLeasable reports whether name is one of the resources returned by Leasable.
func (c *Config) IsLeasable(name string) bool {
	return slices.Contains(c.Leasable(), name)
}
