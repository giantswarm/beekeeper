package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/platform"
)

// reload is the fake service manager's reload command.
const reload = "reload"

const replacementExe = "/new/bin/beekeeper"

// fakeSetup is a service manager whose service is a unit file in the
// home's unit directory and a guard file beside it.
type fakeSetup struct {
	started bool
	ran     []string
}

func (*fakeSetup) Available() bool { return true }

func (*fakeSetup) Files(s platform.SetupSpec) ([]platform.File, []string) {
	dir := filepath.Join(s.ConfigDir, "units")
	files := []platform.File{
		{Path: filepath.Join(dir, "notify.service"), Content: []byte("ExecStart=" + s.Exe + " watch\n"), Service: true},
		{Path: filepath.Join(dir, "guard.d", "guard.conf"), Content: []byte("MemoryMax=1M\n")},
	}
	if s.TeleportEvery > 0 {
		files = append(files, platform.File{Path: filepath.Join(dir, "keeper.timer"), Content: []byte("OnUnitActiveSec=" + s.TeleportEvery.String() + "\n"), Service: true})
	}
	return files, []string{"something: not on this machine"}
}

func (f *fakeSetup) Started(context.Context, string) bool { return f.started }
func (*fakeSetup) Reload() []string                       { return []string{reload} }
func (*fakeSetup) Start(p string) []string                { return []string{"start", filepath.Base(p)} }
func (*fakeSetup) Stop(p string) []string                 { return []string{"stop", filepath.Base(p)} }
func (*fakeSetup) Halt(p string) []string                 { return []string{"halt", filepath.Base(p)} }
func (*fakeSetup) Restart(p string) [][]string            { return [][]string{{"restart", filepath.Base(p)}} }

func (f *fakeSetup) run(_ context.Context, argv []string) error {
	f.ran = append(f.ran, strings.Join(argv, " "))
	switch argv[0] {
	case "start":
		f.started = true
	case "stop", "halt":
		f.started = false
	}
	return nil
}

func env(t *testing.T, home string, setup platform.Setup, run func(context.Context, []string) error) (Env, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	return Env{
		Exe:      "/opt/bin/beekeeper",
		Settings: filepath.Join(home, ".claude", "settings.json"),
		Config:   filepath.Join(home, ".config", "beekeeper", "config.yaml"),
		StateDir: filepath.Join(home, ".local", "state", "beekeeper"),
		Setup:    setup,
		Spec:     platform.SetupSpec{Home: home, ConfigDir: filepath.Join(home, ".config"), Exe: "/opt/bin/beekeeper"},
		Run:      run,
		Out:      out,
	}, out
}

// tree is every file and directory under root with its content.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	fsys := os.DirFS(root)
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		path := filepath.Join(root, p)
		if d.IsDir() {
			m[path+"/"] = ""
			return nil
		}
		raw, err := fs.ReadFile(fsys, p)
		m[path] = string(raw)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// within is the part of a tree under dir.
func within(m map[string]string, dir string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if strings.HasPrefix(k, dir) {
			out[k] = v
		}
	}
	return out
}

const priorSettings = `{
  "model": "opus",
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "echo <&> ünïcode",
            "timeout": 5
          }
        ]
      }
    ]
  },
  "z": 1.50
}
`

func TestInstallTwiceThenUninstallRestores(t *testing.T) {
	home := t.TempDir()
	f := &fakeSetup{}
	e, out := env(t, home, f, f.run)
	write(t, e.Settings, priorSettings)
	write(t, filepath.Join(home, ".config", "units", "other.service"), "theirs\n")
	before := tree(t, home)

	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(e.Settings)
	for _, want := range []string{`"command": "/opt/bin/beekeeper hook pretooluse"`, `"command": "/opt/bin/beekeeper hook permissionrequest"`, `"echo <&> ünïcode"`, `"z": 1.50`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("settings lack %s:\n%s", want, raw)
		}
	}
	if !strings.HasPrefix(string(raw), "{\n  \"model\": \"opus\",\n  \"hooks\"") {
		t.Errorf("settings lost their key order:\n%s", raw)
	}
	if got := strings.Join(f.ran, ","); got != "reload,start notify.service" {
		t.Errorf("ran %s", got)
	}
	if !strings.Contains(out.String(), "skip    something: not on this machine") {
		t.Errorf("no skip line:\n%s", out)
	}

	installed := tree(t, home)
	f.ran = nil
	out.Reset()
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if again := tree(t, home); !maps.Equal(installed, again) {
		t.Errorf("a second install changed files")
	}
	for line := range strings.Lines(out.String()) {
		if v := strings.Fields(line)[0]; v != "ok" && v != "skip" {
			t.Errorf("second install: %s", line)
		}
	}
	if len(f.ran) > 0 {
		t.Errorf("second install ran %v", f.ran)
	}

	if err := Uninstall(context.Background(), e, false); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.ran, ","); got != "stop notify.service,reload" {
		t.Errorf("uninstall ran %s", got)
	}
	after := tree(t, home)
	for _, dir := range []string{filepath.Join(home, ".claude"), filepath.Join(home, ".config", "units")} {
		if !maps.Equal(within(before, dir), within(after, dir)) {
			t.Errorf("uninstall left %s changed:\nbefore %v\nafter  %v", dir, within(before, dir), within(after, dir))
		}
	}
	if _, err := os.Stat(e.Config); err != nil {
		t.Errorf("uninstall without --purge removed the config: %v", err)
	}
	if exists(e.manifest()) {
		t.Errorf("uninstall left the manifest")
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	home := t.TempDir()
	f := &fakeSetup{}
	e, out := env(t, home, f, f.run)
	e.DryRun = true
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, home); len(got) != 1 || len(f.ran) > 0 {
		t.Errorf("install --dry-run wrote %v, ran %v", got, f.ran)
	}
	if !strings.HasPrefix(out.String(), "dry run: nothing changes\n") || !strings.Contains(out.String(), "run     start notify.service") {
		t.Errorf("install --dry-run printed:\n%s", out)
	}

	e.DryRun = false
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	installed := tree(t, home)
	f.ran = nil
	e.DryRun = true
	for _, purge := range []bool{false, true} {
		out.Reset()
		if err := Uninstall(context.Background(), e, purge); err != nil {
			t.Fatal(err)
		}
		if got := tree(t, home); !maps.Equal(installed, got) || len(f.ran) > 0 {
			t.Errorf("uninstall --dry-run (purge %v) changed files or ran %v", purge, f.ran)
		}
		if !strings.Contains(out.String(), "remove  ~/.config/units/notify.service") {
			t.Errorf("uninstall --dry-run printed:\n%s", out)
		}
	}
}

func TestKeepsWhatItDidNotWrite(t *testing.T) {
	home := t.TempDir()
	f := &fakeSetup{started: true}
	e, out := env(t, home, f, f.run)
	service := filepath.Join(home, ".config", "units", "notify.service")
	write(t, service, "ExecStart=/elsewhere/beekeeper watch\n")
	write(t, e.Settings, `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/elsewhere/beekeeper hook pretooluse"}]}]}}`)
	write(t, e.Config, "shell: zsh\n")
	before := tree(t, home)

	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"keep    ~/.config/units/notify.service: differs",
		"keep    ~/.claude/settings.json: PreToolUse hook: a beekeeper hook with another command",
		"add     ~/.claude/settings.json: PermissionRequest hook",
		"ok      ~/.config/beekeeper/config.yaml: a config exists",
		"keep    notify.service: its definition is not beekeeper install's",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("install lacks %q:\n%s", want, out)
		}
	}
	if raw, _ := os.ReadFile(filepath.Clean(service)); string(raw) != before[service] {
		t.Errorf("install changed a unit it did not write")
	}
	if len(f.ran) != 1 || f.ran[0] != reload {
		t.Errorf("install ran %v: a reload for the guard only, nothing for a service it did not write", f.ran)
	}
	f.ran = nil

	if err := Uninstall(context.Background(), e, false); err != nil {
		t.Fatal(err)
	}
	after := tree(t, home)
	delete(after, filepath.Join(home, ".config", "units", "guard.d")+"/")
	for k, v := range before {
		if after[k] != v {
			t.Errorf("uninstall changed %s: %q", k, after[k])
		}
	}
	if len(f.ran) != 1 || f.ran[0] != reload {
		t.Errorf("uninstall ran %v", f.ran)
	}
}

func TestUpdatesWhatItWrote(t *testing.T) {
	home := t.TempDir()
	f := &fakeSetup{}
	e, out := env(t, home, f, f.run)
	before := tree(t, home)
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	e.Exe, e.Spec.Exe = replacementExe, replacementExe
	f.ran = nil
	out.Reset()
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"update  ~/.claude/settings.json: PreToolUse hook", "update  ~/.config/units/notify.service", "run     restart notify.service"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("second install lacks %q:\n%s", want, out)
		}
	}
	raw, _ := os.ReadFile(e.Settings)
	if strings.Contains(string(raw), "/opt/bin") || !strings.Contains(string(raw), "/new/bin/beekeeper hook pretooluse") {
		t.Errorf("settings not moved to the new binary:\n%s", raw)
	}

	if err := Uninstall(context.Background(), e, true); err != nil {
		t.Fatal(err)
	}
	if after := tree(t, home); !maps.Equal(before, after) {
		t.Errorf("uninstall --purge left %v", after)
	}
}

// A machine set up without a manifest (by hand, by self-update): the hook
// entries and the unit of this binary are updated in place, never added a
// second time, and what is not beekeeper's stays as it is.
func TestTakesOverThisBinarysEntries(t *testing.T) {
	home := t.TempDir()
	f := &fakeSetup{started: true}
	e, out := env(t, home, f, f.run)
	service := filepath.Join(home, ".config", "units", "notify.service")
	write(t, service, "# an older unit\nExecStart=/opt/bin/beekeeper watch --notify\n")
	write(t, e.Settings, `{"model":"opus","hooks":{"PreToolUse":[`+
		`{"matcher":"Bash","hooks":[{"type":"command","command":"echo theirs"}]},`+
		`{"matcher":"Bash|Edit","hooks":[{"type":"command","command":"/opt/bin/beekeeper hook pretooluse","timeout":30}]}]}}`)

	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"update  ~/.claude/settings.json: PreToolUse hook",
		"update  ~/.config/units/notify.service: an earlier unit of this binary",
		"run     restart notify.service",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("install lacks %q:\n%s", want, out)
		}
	}
	s, err := readSettings(e.Settings)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := s.hooks("PreToolUse")
	if len(list) != 2 || !sameJSON(list[1], Hooks(e.Exe)[0].Entry) || !strings.Contains(string(list[0]), "echo theirs") {
		t.Errorf("PreToolUse not updated in place:\n%s", list)
	}
	if raw, _ := s.top.get("model"); string(raw) != `"opus"` {
		t.Errorf("install touched an unrelated setting: %s", raw)
	}
	if raw, _ := os.ReadFile(filepath.Clean(service)); string(raw) != "ExecStart=/opt/bin/beekeeper watch\n" {
		t.Errorf("unit not updated: %s", raw)
	}

	installed := tree(t, home)
	f.ran = nil
	out.Reset()
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if again := tree(t, home); !maps.Equal(installed, again) || len(f.ran) > 0 {
		t.Errorf("a second install changed files or ran %v:\n%s", f.ran, out)
	}

	if err := Uninstall(context.Background(), e, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(e.Settings)
	if strings.Contains(string(raw), "beekeeper") || !strings.Contains(string(raw), "echo theirs") {
		t.Errorf("uninstall left the taken-over hooks or removed theirs:\n%s", raw)
	}
}

// A unit path that is a symlink into the person's dotfiles is theirs:
// install leaves the link and its target, a dangling one too, as they are,
// says why, and neither starts, restarts nor records the unit; a unit an
// earlier install recorded before it became a link is dropped from the
// manifest, so uninstall neither stops nor removes it. A regular unit file
// is written and started as before.
func TestRefusesASymlinkedUnit(t *testing.T) {
	const theirs = "# the person's timer\nOnCalendar=*:0/5\n"
	for _, c := range []struct {
		name              string
		dangling, earlier bool
	}{{"target", false, false}, {"dangling", true, false}, {"earlier install", false, true}} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			f := &fakeSetup{}
			e, out := env(t, home, f, f.run)
			e.Spec.TeleportEvery = 5 * time.Minute
			timer := filepath.Join(home, ".config", "units", "keeper.timer")
			target := filepath.Join(home, "dotfiles", "keeper.timer")
			if c.earlier {
				if err := Install(context.Background(), e); err != nil {
					t.Fatal(err)
				}
				if fi, err := os.Lstat(timer); err != nil || !fi.Mode().IsRegular() || !slices.Contains(f.ran, "start keeper.timer") {
					t.Fatalf("a regular unit not written and started: %v, ran %v", err, f.ran)
				}
				if err := os.Remove(timer); err != nil {
					t.Fatal(err)
				}
			}
			if !c.dangling {
				write(t, target, theirs)
			}
			if err := os.MkdirAll(filepath.Dir(timer), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join("..", "..", "dotfiles", "keeper.timer"), timer); err != nil {
				t.Fatal(err)
			}
			f.ran, f.started = nil, true
			out.Reset()

			if err := Install(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			want := "keep    ~/.config/units/keeper.timer: a symlink to ~/dotfiles/keeper.timer: install never writes through a link"
			if !strings.Contains(out.String(), want) {
				t.Errorf("install lacks %q:\n%s", want, out)
			}
			unchanged := func(when string) {
				t.Helper()
				if fi, err := os.Lstat(timer); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
					t.Errorf("%s: the unit path is no longer a symlink: %v", when, err)
				}
				raw, err := os.ReadFile(filepath.Clean(target))
				if c.dangling && !errors.Is(err, fs.ErrNotExist) || !c.dangling && string(raw) != theirs {
					t.Errorf("%s: the link's target changed: %q, %v", when, raw, err)
				}
				for _, ran := range f.ran {
					if strings.HasSuffix(ran, " keeper.timer") {
						t.Errorf("%s: ran %q on the linked unit", when, ran)
					}
				}
			}
			unchanged("install")
			m, err := readManifest(e.manifest())
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := m.Files[timer]; ok || slices.Contains(m.Services, timer) {
				t.Errorf("the manifest records the linked unit: %+v", m)
			}

			if err := Uninstall(context.Background(), e, false); err != nil {
				t.Fatal(err)
			}
			unchanged("uninstall")
		})
	}
}

// A unit an earlier install wrote through the person's symlink is
// beekeeper's content at the person's path: uninstall stops the unit
// without disabling it (systemd removes a linked unit on disable), removes
// the target beekeeper wrote and leaves the link; a dry run names both. A
// target the person changed since is theirs and kept.
func TestUninstallResolvesASymlinkedUnit(t *testing.T) {
	for _, c := range []struct {
		name    string
		changed bool
	}{{"beekeeper's target", false}, {"the person's target", true}} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			f := &fakeSetup{}
			e, out := env(t, home, f, f.run)
			if err := Install(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			unit := filepath.Join(home, ".config", "units", "notify.service")
			target := filepath.Join(home, "dotfiles", "notify.service")
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(unit, target); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join("..", "..", "dotfiles", "notify.service")
			if err := os.Symlink(link, unit); err != nil {
				t.Fatal(err)
			}
			if c.changed {
				write(t, target, "# the person's own\n")
			}
			content, _ := os.ReadFile(filepath.Clean(target))
			linkKept := func(when string) {
				t.Helper()
				if got, err := os.Readlink(unit); err != nil || got != link {
					t.Errorf("%s: the person's link changed: %q, %v", when, got, err)
				}
			}

			f.ran = nil
			out.Reset()
			e.DryRun = true
			if err := Uninstall(context.Background(), e, false); err != nil {
				t.Fatal(err)
			}
			want := "remove  ~/dotfiles/notify.service: beekeeper install wrote it through the symlink ~/.config/units/notify.service, which stays"
			if c.changed {
				want = "keep    ~/.config/units/notify.service: a symlink to ~/dotfiles/notify.service, the person's"
			}
			if !strings.Contains(out.String(), want) {
				t.Errorf("dry run lacks %q:\n%s", want, out)
			}
			if raw, err := os.ReadFile(filepath.Clean(target)); err != nil || !bytes.Equal(raw, content) || len(f.ran) > 0 {
				t.Errorf("the dry run changed something: %q, %v, ran %v", raw, err, f.ran)
			}
			linkKept("dry run")

			e.DryRun = false
			if err := Uninstall(context.Background(), e, false); err != nil {
				t.Fatal(err)
			}
			linkKept("uninstall")
			raw, err := os.ReadFile(filepath.Clean(target))
			switch {
			case c.changed && (err != nil || !bytes.Equal(raw, content)):
				t.Errorf("uninstall changed the person's target: %q, %v", raw, err)
			case !c.changed && !errors.Is(err, fs.ErrNotExist):
				t.Errorf("uninstall left beekeeper's unit at the link's target: %q, %v", raw, err)
			}
			for _, ran := range f.ran {
				if strings.HasPrefix(ran, "stop ") {
					t.Errorf("uninstall disabled the linked unit, which removes the link: %q", ran)
				}
			}
			if halted := slices.Contains(f.ran, "halt notify.service"); halted == c.changed {
				t.Errorf("uninstall ran %v", f.ran)
			}
		})
	}
}

func TestWithoutServiceManager(t *testing.T) {
	home := t.TempDir()
	e, out := env(t, home, platform.Stub().Setup, nil)
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "skip    standby service: not available on ") {
		t.Errorf("install printed:\n%s", out)
	}
	if !exists(e.Settings) || !exists(e.Config) {
		t.Errorf("install wrote no hooks or config without a service manager")
	}
}

func TestStarterIsTheDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	write(t, path, string(Starter()))
	got, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := config.Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the starter config sets keys")
	}
}

// The keeper's timer is a second unit install starts and uninstall stops.
func TestInstallsTheKeeper(t *testing.T) {
	home := t.TempDir()
	f := &fakeSetup{}
	e, out := env(t, home, f, f.run)
	e.Spec.TeleportEvery = 10 * time.Minute
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if want := []string{reload, "start notify.service", "start keeper.timer"}; !reflect.DeepEqual(f.ran, want) {
		t.Fatalf("ran %q, want %q\n%s", f.ran, want, out)
	}
	m, err := readManifest(e.manifest())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Services) != 2 || !strings.HasSuffix(m.Services[1], "keeper.timer") {
		t.Fatalf("services %q", m.Services)
	}
	f.ran = nil
	if err := Uninstall(context.Background(), e, false); err != nil {
		t.Fatal(err)
	}
	if want := []string{"stop notify.service", "stop keeper.timer", reload}; !reflect.DeepEqual(f.ran, want) {
		t.Errorf("uninstall ran %q, want %q", f.ran, want)
	}
}

// A manifest an earlier install wrote names the standby service alone.
func TestReadsTheEarlierManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install.json")
	write(t, path, `{"service": "/u/notify.service"}`)
	m, err := readManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.Services, []string{"/u/notify.service"}) || m.Service != "" {
		t.Errorf("manifest %+v", m)
	}
}

const priorAgyHooks = `{
  "other-guard": {
    "PreToolUse": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "/usr/local/bin/other-guard",
            "timeout": 10
          }
        ]
      }
    ]
  }
}
`

// Install writes and updates the beekeeper entry in agy's hooks.json without
// touching other named hooks in that file; CheckAgyHooks checks it; uninstall
// removes only beekeeper's entry (or the file when install created it and no
// other hook remains).
func TestAgyHooksPreservesOtherNamedHooks(t *testing.T) {
	home := t.TempDir()
	f := &fakeSetup{}
	e, out := env(t, home, f, f.run)
	hooksPath := filepath.Join(home, ".gemini", "config", "hooks.json")

	// When e.AgyHooks is empty (agy not installed), Install writes nothing at ~/.gemini/config/hooks.json.
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if exists(hooksPath) {
		t.Fatalf("Install wrote %s when e.AgyHooks was empty", hooksPath)
	}

	e.AgyHooks = hooksPath
	write(t, e.AgyHooks, priorAgyHooks)

	if got := CheckAgyHooks(e.AgyHooks, e.Exe, home); !strings.Contains(got, "lacks the beekeeper PreToolUse hook") {
		t.Fatalf("before install: CheckAgyHooks = %q", got)
	}
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(e.AgyHooks)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"other-guard"`,
		`"/usr/local/bin/other-guard"`,
		`"beekeeper"`,
		`"/opt/bin/beekeeper hook preinvocation"`,
		`"/opt/bin/beekeeper hook pretooluse --format agy"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("hooks.json lacks %s:\n%s", want, raw)
		}
	}
	if !strings.HasPrefix(string(raw), "{\n  \"other-guard\":") {
		t.Errorf("hooks.json lost key order:\n%s", raw)
	}
	if got := CheckAgyHooks(e.AgyHooks, e.Exe, home); got != "" {
		t.Errorf("after install: CheckAgyHooks = %q, want empty", got)
	}

	// A second Install run is idempotent (`ok`) and leaves hooks.json byte-for-byte unchanged.
	out.Reset()
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ok      ~/.gemini/config/hooks.json: beekeeper PreToolUse hook") {
		t.Errorf("idempotent second run output:\n%s", out)
	}
	raw2, err := os.ReadFile(e.AgyHooks)
	if err != nil || string(raw2) != string(raw) {
		t.Errorf("second run changed hooks.json:\n%s\nwant:\n%s", raw2, raw)
	}
	const updatedExe = "/updated/bin/beekeeper"
	if got := CheckAgyHooks(e.AgyHooks, updatedExe, home); !strings.Contains(got, "runs /opt/bin/beekeeper instead of "+updatedExe) {
		t.Errorf("wrong binary: CheckAgyHooks = %q", got)
	}

	// Missing PreInvocation in beekeeper's hooks.json entry is named by CheckAgyHooks.
	missingPre := filepath.Join(home, "missing-pre.json")
	write(t, missingPre, `{"beekeeper":{"PreToolUse":[{"hooks":[{"type":"command","command":"/opt/bin/beekeeper hook pretooluse --format agy","timeout":30}]}]}}`)
	if got := CheckAgyHooks(missingPre, e.Exe, home); !strings.Contains(got, "PreInvocation hook differs") {
		t.Errorf("missing PreInvocation: CheckAgyHooks = %q", got)
	}
	_ = os.Remove(missingPre)

	// Updating to a new binary updates beekeeper's entry in place and keeps other-guard.
	e.Exe, e.Spec.Exe = updatedExe, updatedExe
	out.Reset()
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "update  ~/.gemini/config/hooks.json: beekeeper PreToolUse hook") {
		t.Errorf("update output:\n%s", out)
	}
	raw, _ = os.ReadFile(e.AgyHooks)
	if strings.Contains(string(raw), "/opt/bin") || !strings.Contains(string(raw), updatedExe+" hook preinvocation") || !strings.Contains(string(raw), updatedExe+" hook pretooluse --format agy") || !strings.Contains(string(raw), `"other-guard"`) {
		t.Errorf("hooks.json after update:\n%s", raw)
	}
	if got := CheckAgyHooks(e.AgyHooks, e.Exe, home); got != "" {
		t.Errorf("after update: CheckAgyHooks = %q, want empty", got)
	}

	// Uninstall removes beekeeper's entry and leaves other-guard intact.
	if err := Uninstall(context.Background(), e, false); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(e.AgyHooks)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != priorAgyHooks {
		t.Errorf("after uninstall:\n%s\nwant:\n%s", raw, priorAgyHooks)
	}

	// Installing when hooks.json does not exist creates it, and uninstall removes it.
	if err := os.Remove(e.AgyHooks); err != nil {
		t.Fatal(err)
	}
	if got := CheckAgyHooks(e.AgyHooks, e.Exe, home); !strings.Contains(got, "is missing") {
		t.Errorf("missing file: CheckAgyHooks = %q", got)
	}
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if got := CheckAgyHooks(e.AgyHooks, e.Exe, home); got != "" {
		t.Errorf("fresh install: CheckAgyHooks = %q", got)
	}
	if err := Uninstall(context.Background(), e, true); err != nil {
		t.Fatal(err)
	}
	if exists(e.AgyHooks) {
		t.Errorf("uninstall left hooks.json it created")
	}
}

// rejectedAgyHook is the beekeeper entry earlier installs wrote: its
// PreInvocation item is a matcher entry, so agy refuses the whole entry
// ("command hook must specify 'command'") and neither hook runs.
const rejectedAgyHook = `{
  "beekeeper": {
    "PreInvocation": [{"matcher": "*", "hooks": [{"type": "command", "command": "/opt/bin/beekeeper hook preinvocation", "timeout": 30}]}],
    "PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "/opt/bin/beekeeper hook pretooluse --format agy", "timeout": 30}]}]
  }
}
`

// The entry install writes for agy lists the PreInvocation command itself,
// as agy reads that event, and PreToolUse as a matcher with its command; the
// doctor fails on the entry earlier installs wrote, which agy rejects, and
// install replaces it.
func TestAgyHookIsTheShapeAgyAccepts(t *testing.T) {
	var events struct {
		PreInvocation []map[string]any `json:"PreInvocation"`
		PreToolUse    []struct {
			Matcher string           `json:"matcher"`
			Hooks   []map[string]any `json:"hooks"`
		} `json:"PreToolUse"`
	}
	if err := json.Unmarshal(AgyHook("/opt/bin/beekeeper"), &events); err != nil {
		t.Fatal(err)
	}
	if len(events.PreInvocation) != 1 || events.PreInvocation[0]["type"] != "command" ||
		events.PreInvocation[0]["command"] != "/opt/bin/beekeeper hook preinvocation" ||
		events.PreInvocation[0]["matcher"] != nil || events.PreInvocation[0]["hooks"] != nil {
		t.Errorf("PreInvocation = %v, want the one command itself", events.PreInvocation)
	}
	if len(events.PreToolUse) != 1 || events.PreToolUse[0].Matcher != "*" || len(events.PreToolUse[0].Hooks) != 1 ||
		events.PreToolUse[0].Hooks[0]["command"] != "/opt/bin/beekeeper hook pretooluse --format agy" {
		t.Errorf("PreToolUse = %+v, want matcher * with the guard", events.PreToolUse)
	}

	home := t.TempDir()
	f := &fakeSetup{}
	e, out := env(t, home, f, f.run)
	e.AgyHooks = filepath.Join(home, ".gemini", "config", "hooks.json")
	write(t, e.AgyHooks, rejectedAgyHook)
	if got := CheckAgyHooks(e.AgyHooks, e.Exe, home); !strings.Contains(got, "agy rejects the beekeeper entry") ||
		!strings.Contains(got, "PreInvocation: command hook must specify 'command'") {
		t.Errorf("rejected entry: CheckAgyHooks = %q", got)
	}
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "update  ~/.gemini/config/hooks.json: beekeeper PreToolUse hook") {
		t.Errorf("install output:\n%s", out)
	}
	if got := CheckAgyHooks(e.AgyHooks, e.Exe, home); got != "" {
		t.Errorf("after install: CheckAgyHooks = %q, want empty", got)
	}
	raw, err := os.ReadFile(e.AgyHooks)
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]json.RawMessage
	if err := json.Unmarshal(raw, &file); err != nil || !sameJSON(file["beekeeper"], AgyHook(e.Exe)) {
		t.Errorf("hooks.json after install:\n%s", raw)
	}
}

// Install never writes agy's hooks.json through a link, as it never writes
// skills.json through one: it keeps the link and its target, and the doctor
// names the target to edit.
func TestAgyHooksNeverWritesThroughALink(t *testing.T) {
	home := t.TempDir()
	f := &fakeSetup{}
	e, out := env(t, home, f, f.run)
	target := filepath.Join(home, "dotfiles", "hooks.json")
	write(t, target, priorAgyHooks)
	e.AgyHooks = filepath.Join(home, ".gemini", "config", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(e.AgyHooks), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, e.AgyHooks); err != nil {
		t.Fatal(err)
	}
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Clean(target)); string(raw) != priorAgyHooks {
		t.Errorf("install wrote through the link:\n%s", raw)
	}
	if !symlink(e.AgyHooks) {
		t.Errorf("install replaced the link")
	}
	if !strings.Contains(out.String(), "keep    ~/.gemini/config/hooks.json") || !strings.Contains(out.String(), "install never writes through a link") {
		t.Errorf("install output:\n%s", out)
	}
	if got := CheckAgyHooks(e.AgyHooks, e.Exe, home); !strings.Contains(got, "lacks the beekeeper PreToolUse hook; edit ~/dotfiles/hooks.json") {
		t.Errorf("linked file: CheckAgyHooks = %q", got)
	}
}

const priorAgySkills = `{
  "inherits": [
    {
      "path": "/shared/skills.json"
    }
  ],
  "entries": [
    {
      "path": "~/custom-skills",
      "exclude": [
        "experimental-.*"
      ]
    }
  ]
}
`

// Install writes the embedded skills and registers their directory in agy's
// skills.json idempotently without touching other entries or inherits;
// CheckAgySkills detects missing or stale entries and skill files; nothing is
// written when agy is not installed; uninstall removes only beekeeper's entry
// and skill files (or the file when install created it).
func TestAgySkillsPreservesOtherEntriesAndDetectsStale(t *testing.T) {
	home := t.TempDir()
	f := &fakeSetup{}
	e, out := env(t, home, f, f.run)
	skillsPath := filepath.Join(home, ".gemini", "config", "skills.json")
	skillsDir := AgySkillsDir(e.StateDir)

	// When e.AgySkills is empty (agy not installed), Install writes neither skills.json nor skillsDir.
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if exists(skillsPath) || exists(skillsDir) {
		t.Fatalf("Install wrote agy skills (%v, %v) when e.AgySkills was empty", exists(skillsPath), exists(skillsDir))
	}

	e.AgySkills = skillsPath
	write(t, e.AgySkills, priorAgySkills)

	if got := CheckAgySkills(e.AgySkills, e.StateDir, home); !strings.Contains(got, "lacks the beekeeper skills entry") {
		t.Fatalf("before install: CheckAgySkills = %q", got)
	}
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(e.AgySkills)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"/shared/skills.json"`,
		`"~/custom-skills"`,
		`"experimental-.*"`,
		`"` + skillsDir + `"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("skills.json lacks %s:\n%s", want, raw)
		}
	}
	if !strings.HasPrefix(string(raw), "{\n  \"inherits\":") {
		t.Errorf("skills.json lost key order:\n%s", raw)
	}
	if got := CheckAgySkills(e.AgySkills, e.StateDir, home); got != "" {
		t.Errorf("after install: CheckAgySkills = %q, want empty", got)
	}

	// A second Install run is idempotent (`ok`) and leaves skills.json byte-for-byte unchanged.
	out.Reset()
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ok      ~/.gemini/config/skills.json: beekeeper skills") {
		t.Errorf("idempotent second run output:\n%s", out)
	}
	raw2, err := os.ReadFile(e.AgySkills)
	if err != nil || string(raw2) != string(raw) {
		t.Errorf("second run changed skills.json:\n%s\nwant:\n%s", raw2, raw)
	}

	// Doctor finds a stale entry pointing to another state directory.
	otherState := filepath.Join(home, ".local", "state", "other-beekeeper")
	if got := CheckAgySkills(e.AgySkills, otherState, home); !strings.Contains(got, "points to ~/.local/state/beekeeper/skills instead of ~/.local/state/other-beekeeper/skills") {
		t.Errorf("stale path: CheckAgySkills = %q", got)
	}

	// Doctor finds a stale skill file in the registered skills directory, and Install updates it in place.
	write(t, filepath.Join(skillsDir, "worker-rules", "SKILL.md"), "---\nname: worker-rules\n---\n# stale\n")
	if got := CheckAgySkills(e.AgySkills, e.StateDir, home); !strings.Contains(got, "serves the skills worker-rules unlike beekeeper") {
		t.Errorf("stale skill content: CheckAgySkills = %q", got)
	}
	out.Reset()
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "update  ~/.gemini/config/skills.json: beekeeper skills") {
		t.Errorf("update stale skill output:\n%s", out)
	}
	if got := CheckAgySkills(e.AgySkills, e.StateDir, home); got != "" {
		t.Errorf("after skill refresh: CheckAgySkills = %q, want empty", got)
	}

	// Uninstall removes beekeeper's entry and skill files, leaving prior entries and inherits intact.
	if err := Uninstall(context.Background(), e, false); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(e.AgySkills)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != priorAgySkills {
		t.Errorf("after uninstall:\n%s\nwant:\n%s", raw, priorAgySkills)
	}
	if exists(skillsDir) {
		t.Errorf("uninstall left skillsDir %s", skillsDir)
	}

	// Installing when skills.json does not exist creates it, and uninstall removes it.
	if err := os.Remove(e.AgySkills); err != nil {
		t.Fatal(err)
	}
	if got := CheckAgySkills(e.AgySkills, e.StateDir, home); !strings.Contains(got, "is missing") {
		t.Errorf("missing file: CheckAgySkills = %q", got)
	}
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if got := CheckAgySkills(e.AgySkills, e.StateDir, home); got != "" {
		t.Errorf("fresh install: CheckAgySkills = %q", got)
	}
	if err := Uninstall(context.Background(), e, true); err != nil {
		t.Fatal(err)
	}
	if exists(e.AgySkills) {
		t.Errorf("uninstall left skills.json it created")
	}
}

const priorCodexHooks = `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": ".*",
        "hooks": [
          {
            "type": "command",
            "command": "/usr/local/bin/other-codex-hook",
            "timeout": 10
          }
        ]
      }
    ]
  }
}
`

func TestCodexHooksAndSkills(t *testing.T) {
	home := t.TempDir()
	f := &fakeSetup{}
	e, out := env(t, home, f, f.run)
	hooksPath := filepath.Join(home, ".codex", "hooks.json")
	skillsPath := filepath.Join(home, ".codex", "skills")
	skillsDir := AgySkillsDir(e.StateDir)

	// When e.CodexHooks and e.CodexSkills are empty (codex not installed), nothing is written.
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if exists(hooksPath) || exists(skillsPath) || exists(skillsDir) {
		t.Fatalf("Install wrote codex hooks/skills when e.CodexHooks/e.CodexSkills were empty")
	}

	e.CodexHooks = hooksPath
	e.CodexSkills = skillsPath
	write(t, e.CodexHooks, priorCodexHooks)
	userAgentsMD := filepath.Join(home, ".codex", "AGENTS.md")
	write(t, userAgentsMD, "# Standing rules\n")
	customSkill := filepath.Join(skillsPath, "custom-user-skill")
	if err := os.MkdirAll(customSkill, 0o750); err != nil {
		t.Fatal(err)
	}

	if got := CheckCodexHooks(e.CodexHooks, e.Exe, home); !strings.Contains(got, "lacks the beekeeper PreToolUse hook") {
		t.Fatalf("before install: CheckCodexHooks = %q", got)
	}
	if got := CheckCodexSkills(e.CodexSkills, e.StateDir, home); !strings.Contains(got, "lacks the") {
		t.Fatalf("before install: CheckCodexSkills = %q", got)
	}

	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(e.CodexHooks)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"/usr/local/bin/other-codex-hook"`,
		`"/opt/bin/beekeeper hook pretooluse --format codex"`,
		`"matcher": ".*"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("codex hooks.json lacks %s:\n%s", want, raw)
		}
	}
	if got := CheckCodexHooks(e.CodexHooks, e.Exe, home); got != "" {
		t.Errorf("after install: CheckCodexHooks = %q, want empty", got)
	}
	if got := CheckCodexHooks(CodexWorkerHooks(e.StateDir), e.Exe, home); got != "" {
		t.Errorf("after install: CheckCodexHooks(worker) = %q, want empty", got)
	}
	if got := CheckCodexSkills(e.CodexSkills, e.StateDir, home); got != "" {
		t.Errorf("after install: CheckCodexSkills = %q, want empty", got)
	}
	if got := CheckCodexSkills(CodexWorkerSkills(e.StateDir), e.StateDir, home); got != "" {
		t.Errorf("after install: CheckCodexSkills(worker) = %q, want empty", got)
	}
	if gotTarget, err := os.Readlink(filepath.Join(e.StateDir, "codex", "home", "AGENTS.md")); err != nil || gotTarget != userAgentsMD {
		t.Errorf("worker AGENTS.md link = %q (%v), want %q", gotTarget, err, userAgentsMD)
	}

	// Second run is idempotent.
	out.Reset()
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ok      ~/.codex/hooks.json: beekeeper PreToolUse hook") ||
		!strings.Contains(out.String(), "ok      ~/.codex/skills: beekeeper skills") {
		t.Errorf("idempotent second run output:\n%s", out)
	}

	// Updating binary path updates hooks.json in place.
	const updatedExe = "/updated/bin/beekeeper"
	if got := CheckCodexHooks(e.CodexHooks, updatedExe, home); !strings.Contains(got, "runs /opt/bin/beekeeper instead of "+updatedExe) {
		t.Errorf("wrong binary: CheckCodexHooks = %q", got)
	}
	e.Exe, e.Spec.Exe = updatedExe, updatedExe
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if got := CheckCodexHooks(e.CodexHooks, e.Exe, home); got != "" {
		t.Errorf("after update: CheckCodexHooks = %q, want empty", got)
	}

	// Stale "*" matcher or narrowed "Bash" matcher is flagged by CheckCodexHooks and fixed by Install.
	for _, badMatcher := range []string{"*", "Bash"} {
		write(t, e.CodexHooks, `{"hooks":{"PreToolUse":[{"matcher":"`+badMatcher+`","hooks":[{"type":"command","command":"`+updatedExe+` hook pretooluse --format codex","timeout":30}]}]}}`)
		if got := CheckCodexHooks(e.CodexHooks, e.Exe, home); !strings.Contains(got, "differs from what install writes") {
			t.Errorf("matcher %q: CheckCodexHooks = %q, want differs from what install writes", badMatcher, got)
		}
		if err := Install(context.Background(), e); err != nil {
			t.Fatal(err)
		}
		if got := CheckCodexHooks(e.CodexHooks, e.Exe, home); got != "" {
			t.Errorf("after fixing matcher %q: CheckCodexHooks = %q, want empty", badMatcher, got)
		}
	}
	// Restore priorCodexHooks plus the installed entry before testing uninstall.
	write(t, e.CodexHooks, priorCodexHooks)
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}

	// Uninstall removes beekeeper's hook and skill links while preserving other-codex-hook and custom-user-skill.
	if err := Uninstall(context.Background(), e, false); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(e.CodexHooks)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != priorCodexHooks {
		t.Errorf("after uninstall:\n%s\nwant:\n%s", raw, priorCodexHooks)
	}
	if !exists(customSkill) {
		t.Errorf("uninstall removed user's custom skill directory")
	}
	if exists(skillsDir) {
		t.Errorf("uninstall left skillsDir %s", skillsDir)
	}

	// When both Agy and Codex skills share <stateDir>/skills, uninstalling Codex
	// skills while Agy still uses <stateDir>/skills preserves <stateDir>/skills.
	e.AgySkills = filepath.Join(home, ".gemini", "config", "skills.json")
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if !exists(skillsDir) {
		t.Fatalf("Install did not create shared skillsDir %s", skillsDir)
	}
	eCodexOnly := e
	eCodexOnly.AgySkills = ""
	if err := Uninstall(context.Background(), eCodexOnly, false); err != nil {
		t.Fatal(err)
	}
	if !exists(skillsDir) {
		t.Errorf("uninstalling Codex skills deleted shared skillsDir %s while Agy still uses it", skillsDir)
	}
	if got := CheckAgySkills(e.AgySkills, e.StateDir, home); got != "" {
		t.Errorf("Agy skills broken after Codex-only uninstall: %q", got)
	}
}

// upgradeSetup puts the broker before the services a first upgrade must finish.
type upgradeSetup struct{ fakeSetup }

func (f *upgradeSetup) Files(s platform.SetupSpec) ([]platform.File, []string) {
	var files []platform.File
	for _, name := range []string{"beekeeper-sandbox.service", "beekeeper-supervisor-loop.service", "beekeeper-teleport.timer"} {
		files = append(files, platform.File{Path: filepath.Join(s.ConfigDir, "units", name), Content: []byte("ExecStart=" + s.Exe + " watch\n"), Service: true})
	}
	return files, nil
}

func TestInstallDefersOnlyRefusedRestart(t *testing.T) {
	f := &upgradeSetup{}
	e, out := env(t, t.TempDir(), f, f.run)
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	e.Exe, e.Spec.Exe = replacementExe, replacementExe
	f.ran = nil
	e.BeforeRestart = func(_ context.Context, name string) error {
		if name == "beekeeper-sandbox.service" {
			return errors.New("unknown request prepare-handover")
		}
		return nil
	}
	e.DryRun = true
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if len(f.ran) != 0 {
		t.Fatal("dry run invoked service manager")
	}
	e.DryRun = false
	out.Reset()
	if err := Install(context.Background(), e); err != nil {
		t.Fatal("refusal aborted the remaining install", err)
	}
	if slices.Contains(f.ran, "restart beekeeper-sandbox.service") {
		t.Fatal("refused broker was restarted")
	}
	for _, name := range []string{"beekeeper-supervisor-loop.service", "beekeeper-teleport.timer"} {
		if !slices.Contains(f.ran, "restart "+name) {
			t.Fatal("later service was skipped", name)
		}
	}
	if !strings.Contains(out.String(), "restart deferred") {
		t.Fatal("refusal was not reported")
	}
}

func TestInstallRevokesPreparationWhenRestartFails(t *testing.T) {
	f := &fakeSetup{}
	e, _ := env(t, t.TempDir(), f, f.run)
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	e.Exe, e.Spec.Exe = replacementExe, replacementExe
	prepared, revoked := false, false
	e.BeforeRestart = func(context.Context, string) error { prepared = true; return nil }
	e.RestartFailed = func(context.Context, string) error { revoked = true; return nil }
	e.Run = func(ctx context.Context, args []string) error {
		if args[0] == "restart" {
			return errors.New("restart failed")
		}
		return f.run(ctx, args)
	}
	if err := Install(context.Background(), e); err == nil {
		t.Fatal("restart failure ignored")
	}
	if !prepared || !revoked {
		t.Fatal("failed restart left its prepared state")
	}
}
