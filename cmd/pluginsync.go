package cmd

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/giantswarm/beekeeper/pkg/project"
	"github.com/giantswarm/beekeeper/plugin"
)

// claudePlugin runs one claude plugin command, its output in the error;
// tests replace it.
var claudePlugin = func(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "claude", append([]string{"plugin"}, args...)...).CombinedOutput() //nolint:gosec // Claude Code's own plugin commands
	if err != nil {
		return fmt.Errorf("claude plugin %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// stalePlugin is an enabled beekeeper plugin whose skills differ from the
// ones this binary embeds.
type stalePlugin struct {
	p installedPlugin
	// skills are the skills it serves otherwise, or not at all.
	skills []string
}

// String is the doctor's line: the plugin and its version, the binary and
// its version, and the commands that bring the plugin up to date.
func (s stalePlugin) String() string {
	bin := "beekeeper " + project.Version()
	if sha := project.GitSHA(); sha != "" {
		bin += " (commit " + sha[:min(len(sha), 7)] + ")"
	}
	return fmt.Sprintf("stale plugin: %s %s serves the skills %s unlike %s: %s, or beekeeper self-update",
		s.p.ID, s.p.Version, strings.Join(s.skills, ", "), bin, refreshCommand(s.p))
}

// isBeekeeper says whether p is the plugin beekeeper ships, of any
// marketplace.
func isBeekeeper(p installedPlugin) bool {
	name, _, _ := strings.Cut(p.ID, "@")
	return name == pluginName && p.InstallPath != ""
}

// stalePlugins are the enabled beekeeper plugins of ps that do not serve
// every skill as this binary embeds it.
func stalePlugins(ps []installedPlugin) []stalePlugin {
	var stale []stalePlugin
	for _, p := range ps {
		if !isBeekeeper(p) || !p.Enabled && !p.ProjectEnabled {
			continue
		}
		s := stalePlugin{p: p}
		for _, n := range plugin.SkillNames() {
			if !servesSkill(p, n) {
				s.skills = append(s.skills, n)
			}
		}
		if len(s.skills) > 0 {
			stale = append(stale, s)
		}
	}
	return stale
}

// marketplace is the marketplace p is installed from.
func marketplace(p installedPlugin) string {
	_, m, _ := strings.Cut(p.ID, "@")
	return m
}

// updateArgs are the claude plugin arguments that update p in its scope.
func updateArgs(p installedPlugin) []string {
	args := []string{"update", p.ID}
	if p.Scope != "" {
		args = append(args, "--scope", p.Scope)
	}
	return args
}

// refreshCommand is the command line that brings p up to its marketplace's
// latest.
func refreshCommand(p installedPlugin) string {
	return "claude plugin marketplace update " + marketplace(p) + " && claude plugin " + strings.Join(updateArgs(p), " ")
}

// syncPlugins brings the installed beekeeper plugins up to their
// marketplace's latest, every one when all (an update installed a new
// binary), else the stale ones, and says each in a line to w. A failure is
// said with the command that fixes it, and fails nothing else.
func syncPlugins(ctx context.Context, w io.Writer, all bool) {
	ps, err := listPlugins(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(w, "The beekeeper plugin stays as it is: %v\n", err)
		return
	}
	var due []installedPlugin
	for _, p := range ps {
		if all && isBeekeeper(p) {
			due = append(due, p)
		}
	}
	if !all {
		for _, s := range stalePlugins(ps) {
			due = append(due, s.p)
		}
	}
	refreshed := map[string]error{}
	var updated []installedPlugin
	for _, p := range due {
		m := marketplace(p)
		if _, done := refreshed[m]; !done {
			refreshed[m] = claudePlugin(ctx, "marketplace", "update", m)
		}
		err := refreshed[m]
		if err == nil {
			err = claudePlugin(ctx, updateArgs(p)...)
		}
		if err != nil {
			_, _ = fmt.Fprintf(w, "The plugin %s stays at %s: %v (%s)\n", p.ID, p.Version, err, refreshCommand(p))
			continue
		}
		updated = append(updated, p)
	}
	if len(updated) == 0 {
		return
	}
	now, _ := listPlugins(ctx)
	for _, p := range updated {
		version := "its marketplace's latest"
		for _, q := range now {
			if q.ID == p.ID && q.Scope == p.Scope {
				version = q.Version
			}
		}
		_, _ = fmt.Fprintf(w, "Updated the plugin %s %s → %s: sessions started from now load its skills.\n", p.ID, p.Version, version)
	}
}
