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
	oldRelease    = "v0.120.0"
)

// The claude plugin commands that bring the plugin up to date, in the user
// scope and in none.
const (
	refreshMarketplace = "marketplace update beekeeper"
	updateUserScope    = "update beekeeper@beekeeper --scope user"
	updateNoScope      = "update beekeeper@beekeeper"
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
	root.SetArgs([]string{selfUpdateCmdName})
	root.SetOut(&out)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("self-update: %v", err)
	}
	return out.String()
}

// A self-update that installs a new binary which does not run (no path
// here) refreshes the marketplace and updates every installed beekeeper
// plugin itself, so the role skills the plugin serves are the ones the new
// binary embeds, and says so.
func TestSelfUpdateRefreshesPlugin(t *testing.T) {
	ran := fakePlugins(t, []installedPlugin{
		{ID: beekeeperID, Version: oldPlugin, Scope: userScope, Enabled: true, InstallPath: pluginCopy(t, "worker-rules")},
		{ID: "other@beekeeper", Version: "1", Enabled: true},
	})
	out := selfUpdate(t, update.Result{Current: oldRelease, Latest: latestRelease, Newer: true, Updated: true})
	want := []string{refreshMarketplace, updateUserScope}
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
	if want := []string{refreshMarketplace, updateNoScope}; !slices.Equal(*ran, want) {
		t.Errorf("a stale plugin: ran %q, want %q", *ran, want)
	}
}

// The new binary of the tests, as its version line names it.
const newBinary = latestRelease + " (commit 2b8a6d5)"

// asBinary makes this test's beekeeper the installed release id with a
// state directory of its own, and returns that directory.
func asBinary(t *testing.T, id string) string {
	t.Helper()
	prev, prevInstalled, prevUnder := binaryIdentity, runsInstalled, syncsUnder
	t.Cleanup(func() { binaryIdentity, runsInstalled, syncsUnder = prev, prevInstalled, prevUnder })
	binaryIdentity = func() string { return id }
	runsInstalled = func() bool { return true }
	syncsUnder = func(string) bool { return true }
	t.Setenv("BEEKEEPER_CONFIG", filepath.Join(t.TempDir(), "none.yaml"))
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	return filepath.Join(state, project.Name)
}

// recordedSync is the binary the state directory dir records as the last
// to sync the plugin, "" for none.
func recordedSync(dir string) string {
	raw, _ := os.ReadFile(filepath.Join(dir, pluginSyncFile)) //nolint:gosec // the test's state directory
	return strings.TrimSpace(string(raw))
}

// beekeeper runs the command line with args and returns its output, both
// streams.
func beekeeper(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	root := New()
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&out)
	if err := root.Execute(); err != nil {
		t.Fatalf("beekeeper %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

// A release without the plugin sync replaces itself and leaves the plugin
// as it was; the first command the new binary runs brings it up to the new
// binary's commit and records that, so the next command does not sync
// again.
func TestNewBinarySyncsPluginAfterAnOldSelfUpdate(t *testing.T) {
	dir := asBinary(t, newBinary)
	ps := []installedPlugin{{ID: beekeeperID, Version: oldPlugin, Scope: userScope, Enabled: true, InstallPath: pluginCopy(t, "worker-rules")}}
	ran := fakePlugins(t, ps)

	// The old binary's self-update: the new binary renamed over it, and
	// nothing after.
	bin := filepath.Join(t.TempDir(), project.Name)
	for name, content := range map[string]string{bin: oldRelease, bin + ".new": latestRelease} {
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(bin+".new", bin); err != nil {
		t.Fatal(err)
	}
	if len(*ran) != 0 {
		t.Fatalf("the old binary's update ran claude plugin %q", *ran)
	}

	out := beekeeper(t, "timer")
	if want := []string{refreshMarketplace, updateUserScope}; !slices.Equal(*ran, want) {
		t.Errorf("the new binary's first command ran claude plugin %q, want %q", *ran, want)
	}
	now, _ := listPlugins(context.Background())
	if now[0].Version != newPlugin || len(stalePlugins(now)) != 0 {
		t.Errorf("plugin after the first command: %+v, want %s serving this binary's skills", now[0], newPlugin)
	}
	if !strings.Contains(out, "beekeeper@beekeeper 50e1b9d0aaaa → 2b8a6d5b0000") {
		t.Errorf("the first command did not say the plugin's update:\n%s", out)
	}
	if got := recordedSync(dir); got != newBinary {
		t.Errorf("recorded sync %q, want %q", got, newBinary)
	}

	*ran = nil
	beekeeper(t, "timer")
	if len(*ran) != 0 {
		t.Errorf("a second command synced again: %q", *ran)
	}
}

// A Claude Code that does not list its plugins leaves the sync to the next
// command, unsaid; a development build never syncs.
func TestFirstRunSyncWaitsForClaudeCode(t *testing.T) {
	dir := asBinary(t, newBinary)
	prev := listPlugins
	t.Cleanup(func() { listPlugins = prev })
	listPlugins = func(context.Context) ([]installedPlugin, error) { return nil, os.ErrNotExist }
	if out := beekeeper(t, "timer"); strings.Contains(out, "plugin") {
		t.Errorf("a failed plugin list was said:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, pluginSyncFile)); !os.IsNotExist(err) {
		t.Errorf("a sync without the plugin list was recorded: %v", err)
	}

	asBinary(t, "")
	ran := fakePlugins(t, []installedPlugin{{ID: beekeeperID, Version: oldPlugin, Enabled: true, InstallPath: pluginCopy(t, "lease")}})
	beekeeper(t, "timer")
	if len(*ran) != 0 {
		t.Errorf("a development build synced: %q", *ran)
	}
}

// A release binary run from elsewhere than PATH, a downloaded candidate
// under proof, changes no plugin and records no sync, neither on its first
// command nor asked to sync; the installed binary of the same release
// still syncs on its first command.
func TestACandidateBinaryLeavesThePluginAlone(t *testing.T) {
	dir := asBinary(t, newBinary)
	runsInstalled = func() bool { return false }
	ran := fakePlugins(t, []installedPlugin{{ID: beekeeperID, Version: oldPlugin, Scope: userScope, Enabled: true, InstallPath: pluginCopy(t, "worker-rules")}})

	beekeeper(t, "timer")
	out := beekeeper(t, selfUpdateCmdName, "--"+syncPluginsFlag)
	if len(*ran) != 0 {
		t.Errorf("a candidate binary ran claude plugin %q", *ran)
	}
	if got := recordedSync(dir); got != "" {
		t.Errorf("a candidate binary recorded the sync %q", got)
	}
	if !strings.Contains(out, "not the one on PATH") {
		t.Errorf("a candidate asked to sync did not say why it does not:\n%s", out)
	}

	runsInstalled = func() bool { return true }
	beekeeper(t, "timer")
	if want := []string{refreshMarketplace, updateUserScope}; !slices.Equal(*ran, want) {
		t.Errorf("the installed binary's first command ran claude plugin %q, want %q", *ran, want)
	}
	if got := recordedSync(dir); got != newBinary {
		t.Errorf("recorded sync %q, want %q", got, newBinary)
	}
}

// The installed binary is the beekeeper on PATH, a symbolic link to it
// included; another file of that name on PATH, or none, makes the running
// one a binary from elsewhere.
func TestRunsInstalledIsTheBeekeeperOnPath(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	linked := t.TempDir()
	if err := os.Symlink(self, filepath.Join(linked, project.Name)); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, project.Name), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // an executable stand-in
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{linked, true},
		{other, false},
		{t.TempDir(), false},
	} {
		t.Setenv("PATH", tc.path)
		if got := runsInstalled(); got != tc.want {
			t.Errorf("PATH %s: runsInstalled() = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// A self-update that installs a new binary hands the plugin sync to it:
// the installed binary runs its own sync, once, and this one syncs nothing
// itself; an installed binary that does not run leaves the sync to this
// one.
func TestSelfUpdateHandsPluginSyncToTheNewBinary(t *testing.T) {
	dir := asBinary(t, newBinary)
	ran := fakePlugins(t, []installedPlugin{{ID: beekeeperID, Version: oldPlugin, Scope: userScope, Enabled: true, InstallPath: pluginCopy(t, "worker-rules")}})
	const path = "/usr/local/bin/beekeeper"
	var handed [][]string
	prev := runInstalled
	t.Cleanup(func() { runInstalled = prev })
	runInstalled = func(_ context.Context, w io.Writer, p string, args ...string) error {
		handed = append(handed, append([]string{p}, args...))
		_, _ = io.WriteString(w, beekeeper(t, args...))
		return nil
	}
	res := update.Result{Current: oldRelease, Latest: latestRelease, Newer: true, Updated: true, Path: path}
	out := selfUpdate(t, res)
	if want := []string{path, selfUpdateCmdName, "--" + syncPluginsFlag}; len(handed) != 1 || !slices.Equal(handed[0], want) {
		t.Errorf("self-update ran %q, want %q", handed, want)
	}
	if want := []string{refreshMarketplace, updateUserScope}; !slices.Equal(*ran, want) {
		t.Errorf("claude plugin ran %q, want %q once, by the new binary", *ran, want)
	}
	if !strings.Contains(out, "50e1b9d0aaaa → 2b8a6d5b0000") {
		t.Errorf("self-update did not say the new binary's sync:\n%s", out)
	}
	if got := recordedSync(dir); got != newBinary {
		t.Errorf("recorded sync %q, want %q", got, newBinary)
	}

	ran = fakePlugins(t, []installedPlugin{{ID: beekeeperID, Version: oldPlugin, Enabled: true, InstallPath: pluginCopy(t, "lease")}})
	runInstalled = func(context.Context, io.Writer, string, ...string) error { return os.ErrPermission }
	out = selfUpdate(t, res)
	if want := []string{refreshMarketplace, updateNoScope}; !slices.Equal(*ran, want) {
		t.Errorf("a new binary that does not run: ran %q, want %q", *ran, want)
	}
	if !strings.Contains(out, "did not sync its plugin") {
		t.Errorf("self-update did not say the new binary failed:\n%s", out)
	}
}
