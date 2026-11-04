package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
)

func TestIndividualConfigSymlinkTargetIsReadable(t *testing.T) {
	home := testHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	configDir := filepath.Join(home, ".config/beekeeper")
	targetDir := filepath.Join(home, ".dotfiles")
	for _, dir := range []string{configDir, targetDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(targetDir, "beekeeper.yaml")
	neighbor := filepath.Join(targetDir, "neighbor.yaml")
	for _, path := range []string{target, neighbor} {
		if err := os.WriteFile(path, []byte("# public fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(configDir, "config.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	p := New(config.Sandbox{}, Paths{Home: home, ConfigFile: link,
		StateDir: filepath.Join(home, ".local/state/beekeeper"), Exe: filepath.Join(home, ".bin/beekeeper")})
	if got := p.Vars["BEEKEEPER_CONFIG"]; got != target || !p.Readable(got, home) {
		t.Errorf("config target is not readable: %q", got)
	}
	filesystem := p.Settings()["sandbox"].(map[string]any)["filesystem"].(map[string]any)
	if allowed := filesystem["allowRead"].([]string); !slices.Contains(allowed, p.setting(target)) {
		t.Errorf("managed allowRead %v lacks the individual target", allowed)
	}
	if p.Readable(neighbor, home) || p.Readable(targetDir, home) {
		t.Error("config target's neighbors were opened")
	}
	if p.Writable(target, home) {
		t.Error("config target became writable")
	}
}
