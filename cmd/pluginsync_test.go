package cmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/update"
	"github.com/giantswarm/beekeeper/pkg/project"
	"github.com/giantswarm/beekeeper/plugin"
)

// The plugin, its versions (marketplace commits) and its scope in the tests.
const (
	beekeeperID   = "beekeeper@beekeeper"
	oldPlugin     = "50e1b9d0aaaa"
	newPlugin     = "2b8a6d5b0000"
	userScope     = "user"
	latestRelease = "v0.121.0"
)

// pluginCopy writes this binary's skills into a plugin cache folder, the
// named ones changed, as an older marketplace commit installed them.
func pluginCopy(t *testing.T, stale ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range plugin.SkillNames() {
		raw := plugin.SkillFile(n)
		if slices.Contains(stale, n) {
			raw = []byte("---\nname: " + n + "\n---\n\nthe rules of nine days ago\n")
		}
		p := filepath.Join(dir, plugin.SkillsDir, n, "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Doctor names an installed beekeeper plugin whose skills differ from the
// binary's, with the plugin's version, the binary's and the commands that fix
// it; a current, a disabled or another plugin goes unnamed.
func TestStalePlugins(t *testing.T) {
	stale := installedPlugin{ID: beekeeperID, Version: oldPlugin, Scope: userScope, Enabled: true, InstallPath: pluginCopy(t, "worker-rules", "register-agent")}
	ps := []installedPlugin{
		stale,
		{ID: "beekeeper@org", Version: "1f7316cd2351", Enabled: true, InstallPath: pluginCopy(t)},
		{ID: "beekeeper@old", Version: "0000000aaaaa", InstallPath: pluginCopy(t, "worker-rules")},
		{ID: "other@beekeeper", Version: "1", Enabled: true, InstallPath: pluginCopy(t, "worker-rules")},
	}
	got := stalePlugins(ps)
	if len(got) != 1 || got[0].p.ID != stale.ID || !slices.Equal(got[0].skills, []string{"register-agent", "worker-rules"}) {
		t.Fatalf("stalePlugins = %+v, want beekeeper@beekeeper with register-agent and worker-rules", got)
	}
	line := got[0].String()
	for _, want := range []string{"beekeeper@beekeeper 50e1b9d0aaaa", "register-agent, worker-rules", project.Version(),
		"claude plugin marketplace update beekeeper && claude plugin update beekeeper@beekeeper --scope user", "beekeeper self-update"} {
		if !strings.Contains(line, want) {
			t.Errorf("doctor line lacks %q:\n%s", want, line)
		}
	}
	missing := installedPlugin{ID: beekeeperID, Version: "1", Enabled: true, InstallPath: t.TempDir()}
	if got := stalePlugins([]installedPlugin{missing}); len(got) != 1 || len(got[0].skills) != len(plugin.SkillNames()) {
		t.Errorf("a plugin without the skills: %+v, want every skill named", got)
	}
}

// fakePlugins answers claude plugin list with ps and records every other
// claude plugin command, which brings each listed beekeeper plugin up to
// this binary's skills at its update.
func fakePlugins(t *testing.T, ps []installedPlugin) *[]string {
	t.Helper()
	var ran []string
	prevList, prevRun := listPlugins, claudePlugin
	t.Cleanup(func() { listPlugins, claudePlugin = prevList, prevRun })
	listPlugins = func(context.Context) ([]installedPlugin, error) { return slices.Clone(ps), nil }
	claudePlugin = func(_ context.Context, args ...string) error {
		ran = append(ran, strings.Join(args, " "))
		if args[0] == "update" {
			for i := range ps {
				if ps[i].ID == args[1] {
					ps[i].Version, ps[i].InstallPath = newPlugin, pluginCopy(t)
				}
			}
		}
		return nil
	}
	return &ran
}

// selfUpdate runs beekeeper self-update against an update that returns res.
func selfUpdate(t *testing.T, res update.Result) string {
	t.Helper()
	t.Setenv("BEEKEEPER_CONFIG", filepath.Join(t.TempDir(), "none.yaml"))
	prev := runUpdate
	t.Cleanup(func() { runUpdate = prev })
	runUpdate = func(context.Context, io.Writer, bool) (update.Result, error) { return res, nil }
	var out bytes.Buffer
	root := New()
	root.SetArgs([]string{"self-update"})
	root.SetOut(&out)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("self-update: %v", err)
	}
	return out.String()
}

// A self-update that installs a new binary refreshes the marketplace and
// updates every installed beekeeper plugin with it, so the role skills the
// plugin serves are the ones the new binary embeds, and says so.
func TestSelfUpdateRefreshesPlugin(t *testing.T) {
	ran := fakePlugins(t, []installedPlugin{
		{ID: beekeeperID, Version: oldPlugin, Scope: userScope, Enabled: true, InstallPath: pluginCopy(t, "worker-rules")},
		{ID: "other@beekeeper", Version: "1", Enabled: true},
	})
	out := selfUpdate(t, update.Result{Current: "v0.120.0", Latest: latestRelease, Newer: true, Updated: true})
	want := []string{"marketplace update beekeeper", "update beekeeper@beekeeper --scope user"}
	if !slices.Equal(*ran, want) {
		t.Errorf("self-update ran claude plugin %q, want %q", *ran, want)
	}
	if !strings.Contains(out, "beekeeper@beekeeper 50e1b9d0aaaa → 2b8a6d5b0000") {
		t.Errorf("self-update did not say the plugin's update:\n%s", out)
	}
}

// A self-update on the latest release refreshes a plugin that differs from
// the binary, and leaves a current one alone.
func TestSelfUpdateLatestRefreshesOnlyStalePlugin(t *testing.T) {
	latest := update.Result{Current: latestRelease, Latest: latestRelease}
	ran := fakePlugins(t, []installedPlugin{{ID: beekeeperID, Version: newPlugin, Enabled: true, InstallPath: pluginCopy(t)}})
	selfUpdate(t, latest)
	if len(*ran) != 0 {
		t.Errorf("a current plugin was refreshed: %q", *ran)
	}
	ran = fakePlugins(t, []installedPlugin{{ID: beekeeperID, Version: oldPlugin, Enabled: true, InstallPath: pluginCopy(t, "lease")}})
	selfUpdate(t, latest)
	if want := []string{"marketplace update beekeeper", "update beekeeper@beekeeper"}; !slices.Equal(*ran, want) {
		t.Errorf("a stale plugin: ran %q, want %q", *ran, want)
	}
}
