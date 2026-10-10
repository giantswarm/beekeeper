package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// workerRole names the profile of the workers agents start creates
// (agents.profile), beside the relayed roles' own (supervisor.profile,
// guide.profile).
const workerRole = "worker"

// profileName is the profile the role's sessions get: agents.profile for a
// worker, the relayed role's own for a supervisor or guide; empty: none.
func profileName(cfg *config.Config, role string) string {
	if role == workerRole {
		return cfg.Agents.Profile
	}
	for _, rl := range roles {
		if rl.name == role {
			return rl.cfg(cfg).Profile
		}
	}
	return ""
}

// agentRole is the role whose profile a wake of p gets: the relayed role p
// holds, else a worker's.
func agentRole(st *state.State, p state.Party) string {
	for _, rl := range roles {
		if r := rl.get(st); r.Holder != nil && r.Holder.Is(p) {
			return rl.name
		}
	}
	return workerRole
}

// installedPlugins lists the ids (name@marketplace) of every plugin Claude
// Code has installed, of any marketplace and scope; tests replace it.
var installedPlugins = func(ctx context.Context) ([]string, error) {
	out, err := exec.CommandContext(ctx, "claude", "plugin", "list", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("listing Claude Code's plugins for the profile (claude plugin list --json): %w", err)
	}
	var ps []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &ps); err != nil {
		return nil, fmt.Errorf("reading claude plugin list --json: %w", err)
	}
	ids := make([]string, 0, len(ps))
	for _, p := range ps {
		ids = append(ids, p.ID)
	}
	return ids, nil
}

// profileSettings are a profile's flag settings: every installed plugin it
// does not keep disabled, by id, so a plugin the person's organisation
// syncs under a second marketplace goes too; its MCP servers as the only
// ones allowed; its deny rules.
func profileSettings(p config.Profile, installed []string) map[string]any {
	s := map[string]any{}
	if len(p.Plugins) > 0 {
		off := map[string]bool{}
		for _, id := range installed {
			name, _, _ := strings.Cut(id, "@")
			if !slices.Contains(p.Plugins, id) && !slices.Contains(p.Plugins, name) {
				off[id] = false
			}
		}
		s["enabledPlugins"] = off
	}
	if len(p.MCPServers) > 0 {
		allowed := make([]map[string]string, 0, len(p.MCPServers))
		for _, n := range p.MCPServers {
			allowed = append(allowed, map[string]string{"serverName": n})
		}
		s["allowedMcpServers"] = allowed
	}
	if len(p.Deny) > 0 {
		s["permissions"] = map[string]any{"deny": p.Deny}
	}
	return s
}

// profileFlags writes the named profile's flag settings to beekeeper's
// state folder and returns the flags that give a headless turn the profile:
// its built-in tools and the settings file, which Claude Code ranks over the
// person's user and project settings for that process alone.
func (a *app) profileFlags(ctx context.Context, name string) ([]string, error) {
	p, ok := a.cfg.Agents.ProfileFor(name)
	if !ok {
		return nil, fmt.Errorf("profile %q: no such profile", name)
	}
	var installed []string
	if len(p.Plugins) > 0 {
		var err error
		if installed, err = installedPlugins(ctx); err != nil {
			return nil, err
		}
	}
	raw, err := json.MarshalIndent(profileSettings(p, installed), "", "  ")
	if err != nil {
		return nil, err
	}
	// the configuration's load refuses a profile name that is no plain
	// file name
	dir := filepath.Join(a.cfg.StateDir, "profiles")
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // beekeeper's state folder
		return nil, err
	}
	// one file per profile, rewritten whole: a wake reads the plugins
	// installed at its own start
	path := filepath.Join(dir, name+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil { //nolint:gosec // a validated profile name in beekeeper's state folder
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil { //nolint:gosec // a validated profile name in beekeeper's state folder
		return nil, err
	}
	var flags []string
	if len(p.Tools) > 0 {
		flags = append(flags, toolsFlag, strings.Join(p.Tools, ","))
	}
	return append(flags, settingsFlag, path), nil
}
