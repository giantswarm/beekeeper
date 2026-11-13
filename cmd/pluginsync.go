package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/install"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/pkg/project"
	"github.com/giantswarm/beekeeper/plugin"
)

// claudePlugin runs one claude plugin command, its output in the error;
// tests replace it.
var claudePlugin = func(ctx context.Context, args ...string) error {
	out, err := proc.CommandContext(ctx, "claude", append([]string{"plugin"}, args...)...).CombinedOutput() //nolint:gosec // Claude Code's own plugin commands
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

// syncPlugins brings the installed beekeeper plugins to this binary's
// release tag, every one when all (an update installed a new
// binary), else the stale ones, and says each in a line to w. A failure is
// said with the command that fixes it, and fails nothing else.
func syncPlugins(ctx context.Context, w io.Writer, all bool) {
	if !syncsUnder("") || !runsInstalled() {
		return
	}
	ps, err := listPlugins(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(w, "The beekeeper plugin stays as it is: %v\n", err)
		return
	}
	syncListed(ctx, w, ps, all)
}

// syncListed is syncPlugins of the listed plugins ps.
func syncListed(ctx context.Context, w io.Writer, ps []installedPlugin, all bool) bool {
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
	success := true
	refreshed := map[string]error{}
	var updated []installedPlugin
	for _, p := range due {
		m := marketplace(p)
		if _, done := refreshed[m]; !done {
			refreshed[m] = pinMarketplace(ctx, m, pluginRevision())
			if refreshed[m] == nil {
				refreshed[m] = claudePlugin(ctx, "marketplace", "update", m)
			}
		}
		err := refreshed[m]
		if err == nil {
			err = claudePlugin(ctx, updateArgs(p)...)
		}
		if err != nil {
			_, _ = fmt.Fprintf(w, "The plugin %s stays at %s: %v (%s)\n", p.ID, p.Version, err, refreshCommand(p))
			success = false
			continue
		}
		updated = append(updated, p)
	}
	if len(updated) == 0 {
		return success
	}
	now, err := listPlugins(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(w, "Could not verify the updated plugins: %v\n", err)
		return false
	}
	for _, p := range updated {
		version := ""
		verified := false
		for _, q := range now {
			if q.ID == p.ID && q.Scope == p.Scope {
				version = q.Version
				verified = true
				// Release sync also updates disabled plugins. Verify their
				// installed content without requiring them to be enabled;
				// q is a copy, so this does not change Claude's settings.
				q.Enabled = true
				for _, name := range plugin.SkillNames() {
					verified = verified && servesSkill(q, name)
				}
			}
		}
		if !verified {
			_, _ = fmt.Fprintf(w, "The plugin %s did not verify against this release; its next command will retry.\n", p.ID)
			success = false
			continue
		}
		if version == p.Version {
			continue
		}
		_, _ = fmt.Fprintf(w, "Updated the plugin %s %s → %s: sessions started from now load its skills.\n", p.ID, p.Version, version)
	}
	return success
}

// pluginSyncFile, in the state directory, names the binary that last
// brought the beekeeper plugins up to date.
const pluginSyncFile = "plugin-sync"

// firstRunTimeout bounds the plugin sync a command runs first.
const firstRunTimeout = 2 * time.Minute

// binaryIdentity is this binary's version line, "" for a development build;
// tests replace it.
var binaryIdentity = func() string {
	if project.Version() == "dev" {
		return ""
	}
	return project.VersionLine()
}

// runsInstalled says whether this process runs the installed beekeeper,
// the one on PATH that a self-update replaces, and not a build a unit's
// PATH leads to (devBuildEnv); tests replace it.
var runsInstalled = func() bool {
	if os.Getenv(devBuildEnv) != "" {
		return false
	}
	_, elsewhere := devBuild()
	return !elsewhere
}

// syncsUnder says whether a command under the configuration file config
// (explicitConfig, "" for the default one) syncs the user-wide plugins: a
// scratch configuration leaves them alone; tests replace it.
const (
	xdgStateHomeEnv  = "XDG_STATE_HOME"
	xdgConfigHomeEnv = "XDG_CONFIG_HOME"
)

var syncsUnder = func(config string) bool {
	if config != "" {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	for key, normal := range map[string]string{
		xdgStateHomeEnv:  filepath.Join(home, ".local", "state"),
		xdgConfigHomeEnv: filepath.Join(home, ".config"),
	} {
		if value := os.Getenv(key); value != "" && filepath.Clean(value) != normal {
			return false
		}
	}
	return true
}

// pluginRevision uses the release tag, including a candidate suffix, never
// the marketplace's default branch. Development builds have no release tag.
func pluginRevision() string {
	fields := strings.Fields(binaryIdentity())
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// pinMarketplace asks Claude to register the release source itself. Writing the
// private registry directly can leave a marketplace that Claude does not load.
var pinMarketplace = func(ctx context.Context, name, revision string) error {
	if revision == "" {
		return fmt.Errorf("no release tag for this binary")
	}
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, ".claude")
	}
	path := filepath.Join(dir, "plugins", "known_marketplaces.json")
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return err
	}
	var entries map[string]struct {
		Source struct {
			Kind string `json:"source"`
			Repo string `json:"repo"`
			URL  string `json:"url"`
		} `json:"source"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return err
	}
	entry, ok := entries[name]
	if !ok {
		return fmt.Errorf("marketplace %s is not registered", name)
	}
	var source string
	switch entry.Source.Kind {
	case "github":
		source = entry.Source.Repo
	case "git":
		source = entry.Source.URL
	default:
		return fmt.Errorf("marketplace %s does not support release tags", name)
	}
	if source == "" || strings.Contains(source, "#") {
		return fmt.Errorf("marketplace %s has an invalid source", name)
	}
	if err := pinMarketplaceSettings(dir, name, revision); err != nil {
		return err
	}
	if err := claudePlugin(ctx, "marketplace", "add", source+"#"+revision); err != nil {
		return err
	}
	return verifyMarketplace(ctx, name)
}

// pinMarketplaceSettings aligns an existing user declaration with the release
// before Claude checks marketplace admission. Leave undeclared sources alone.
func pinMarketplaceSettings(dir, name, revision string) error {
	path := filepath.Join(dir, "settings.json")
	raw, err := os.ReadFile(filepath.Clean(path)) //nolint:gosec // user-selected Claude config directory
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settings); err != nil {
		return err
	}
	var marketplaces map[string]map[string]json.RawMessage
	if err := json.Unmarshal(settings["extraKnownMarketplaces"], &marketplaces); err != nil {
		if settings["extraKnownMarketplaces"] == nil {
			return nil
		}
		return err
	}
	entry := marketplaces[name]
	if entry == nil || entry["source"] == nil {
		return nil
	}
	var source map[string]json.RawMessage
	if err := json.Unmarshal(entry["source"], &source); err != nil {
		return err
	}
	if source == nil {
		return fmt.Errorf("marketplace %s has no settings source", name)
	}
	source["ref"], _ = json.Marshal(revision)
	delete(source, "sha")
	entry["source"], err = json.Marshal(source)
	if err != nil {
		return err
	}
	settings["extraKnownMarketplaces"], err = json.Marshal(marketplaces)
	if err != nil {
		return err
	}
	raw, err = json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	info, err := os.Stat(path) //nolint:gosec // user-selected Claude settings path
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".beekeeper-settings-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() //nolint:gosec // temporary file created in the Claude config directory
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path) //nolint:gosec // atomic replacement of the user-selected Claude settings
}

// verifyMarketplace checks Claude's view rather than trusting its registry file.
var verifyMarketplace = func(ctx context.Context, name string) error {
	out, err := proc.CommandContext(ctx, "claude", "plugin", "marketplace", "list", "--json").Output()
	if err != nil {
		return fmt.Errorf("listing Claude marketplaces: %w", err)
	}
	var entries []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &entries); err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name == name {
			return nil
		}
	}
	return fmt.Errorf("marketplace %s is not visible to Claude", name)
}

// syncOnce brings the installed beekeeper plugins up to date once per
// binary: the first command a newly installed binary runs updates every one
// as a self-update does, whatever installed the binary (a self-update by a
// release without the sync, a download, go install), and records the binary
// in dir. Only the installed binary syncs: one run from elsewhere (a
// downloaded release candidate under proof, a build) leaves the plugins and
// the record as they are. A Claude Code that does not list its plugins (none
// installed, a server) leaves the sync to the next command, unsaid; a
// process that finds another syncing goes on without. ran says whether this call synced.
func syncOnce(ctx context.Context, w io.Writer, dir string) (ran bool) {
	id := binaryIdentity()
	if id == "" || dir == "" || !runsInstalled() || !syncsUnder("") {
		return false
	}
	if err := install.RefreshSkills(dir); err != nil {
		_, _ = fmt.Fprintf(w, "Could not refresh harness skills: %v\n", err)
		return false
	}
	path := filepath.Join(dir, pluginSyncFile)
	if synced(path, id) {
		return false
	}
	lock := flock.New(path + ".lock")
	if ok, err := lock.TryLock(); err != nil || !ok {
		return false
	}
	defer func() { _ = lock.Unlock() }()
	if synced(path, id) {
		return false
	}
	ps, err := listPlugins(ctx)
	if err != nil {
		return false
	}
	if !syncListed(ctx, w, ps, true) {
		return true
	}
	_ = os.WriteFile(path, []byte(id+"\n"), 0o600)
	return true
}

// synced says whether the binary id brought the plugins up to date, as the
// file at path records.
func synced(path, id string) bool {
	raw, err := os.ReadFile(path) //nolint:gosec // the state directory's own file
	return err == nil && string(bytes.TrimSpace(raw)) == id
}
