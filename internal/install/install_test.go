package install

import (
	"bytes"
	"context"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/platform"
)

// reload is the fake service manager's reload command.
const reload = "reload"

// fakeSetup is a service manager whose service is a unit file in the
// home's unit directory and a guard file beside it.
type fakeSetup struct {
	started bool
	ran     []string
}

func (*fakeSetup) Available() bool { return true }

func (*fakeSetup) Files(s platform.SetupSpec) ([]platform.File, []string) {
	dir := filepath.Join(s.ConfigDir, "units")
	return []platform.File{
		{Path: filepath.Join(dir, "notify.service"), Content: []byte("ExecStart=" + s.Exe + " watch\n"), Service: true},
		{Path: filepath.Join(dir, "guard.d", "guard.conf"), Content: []byte("MemoryMax=1M\n")},
	}, []string{"something: not on this machine"}
}

func (f *fakeSetup) Started(context.Context, string) bool { return f.started }
func (*fakeSetup) Reload() []string                       { return []string{reload} }
func (*fakeSetup) Start(p string) []string                { return []string{"start", filepath.Base(p)} }
func (*fakeSetup) Stop(p string) []string                 { return []string{"stop", filepath.Base(p)} }

func (f *fakeSetup) run(_ context.Context, argv []string) error {
	f.ran = append(f.ran, strings.Join(argv, " "))
	switch argv[0] {
	case "start":
		f.started = true
	case "stop":
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
		"keep    standby service",
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
	e.Exe, e.Spec.Exe = "/new/bin/beekeeper", "/new/bin/beekeeper"
	f.ran = nil
	out.Reset()
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"update  ~/.claude/settings.json: PreToolUse hook", "update  ~/.config/units/notify.service", "run     stop notify.service", "run     start notify.service"} {
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
