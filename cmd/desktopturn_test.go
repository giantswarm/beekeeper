package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/state"
)

// A turn runs in the desktop only where the desktop can run it: a desktop
// session with a row, the desktop running, its CLI or a steward to take
// the message; otherwise it names why, and the caller's turn runs headless.
func TestTurnInDesktopSaysWhyNot(t *testing.T) {
	a, _ := stubApp(t)
	a.cfg.Claude.DesktopDir = t.TempDir()
	id := "d" + runSixtySeven[1:]
	w := wakeTarget{name: agentTwentyEight, id: id, host: "local_" + id, dir: t.TempDir(), mode: state.ModeBypass}
	if _, err := a.turnInDesktop(t.Context(), wakeTarget{name: w.name, id: id}, "go", 0); err == nil || !strings.Contains(err.Error(), "no desktop session") {
		t.Errorf("no desktop session: %v", err)
	}
	if _, err := a.turnInDesktop(t.Context(), w, "go", 0); err == nil || !strings.Contains(err.Error(), "no row") {
		t.Errorf("no row: %v", err)
	}
	rec := filepath.Join(a.cfg.Claude.DesktopDir, "u", "o", w.host+".json")
	if err := os.MkdirAll(filepath.Dir(rec), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rec, []byte(`{"sessionId":"`+w.host+`","cliSessionId":"`+id+`","cwd":"/tmp","title":"`+w.name+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plat.Machine = tableMachine{plat.Machine}
	if _, err := a.turnInDesktop(t.Context(), w, "go", 0); err == nil || !strings.Contains(err.Error(), "desktop does not run") {
		t.Errorf("desktop not running: %v", err)
	}
	plat.Opener = &recordingOpener{}
	if _, err := a.turnInDesktop(t.Context(), w, "go", 0); err == nil || !strings.Contains(err.Error(), "no idle desktop CLI") {
		t.Errorf("no CLI and no steward: %v", err)
	}
}

// A seed turn has no tools and no MCP servers, and its prompt is the brief
// with the note that it only creates the session.
func TestSeedArgv(t *testing.T) {
	argv := agentArgv("claude", "s1", agentTwentyEight, "", "brief\n\n"+seedNote, "--tools", "", "--strict-mcp-config")
	i := slices.Index(argv, "--tools")
	if i < 0 || argv[i+1] != "" || !slices.Contains(argv, "--strict-mcp-config") {
		t.Fatalf("seed argv %q lacks its tool limits", argv)
	}
	if last := argv[len(argv)-1]; argv[len(argv)-2] != "--" || !strings.HasSuffix(last, seedNote) {
		t.Fatalf("seed prompt %q", last)
	}
}
