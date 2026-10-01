//go:build linux && !nosystemd

package platform

import (
	"strings"
	"testing"
	"time"
)

// testExe is the binary the tested units run.
const testExe = "/b/beekeeper"

func TestSystemdSetupFiles(t *testing.T) {
	files, skipped := systemdSetup{}.Files(SetupSpec{ConfigDir: "/c", Exe: testExe, RAMMiB: 100 << 10, SwapMiB: 16 << 10,
		DesktopScope: "app-Hyprland-com.anthropic.Claude-5776.scope"})
	if len(skipped) > 0 || len(files) != 3 {
		t.Fatalf("files %v, skipped %v", files, skipped)
	}
	for i, want := range []struct{ path, content string }{
		{"/c/systemd/user/beekeeper-notify.service", "ExecStart=/b/beekeeper watch --notify --standby\n"},
		{"/c/systemd/user/memcap.slice", "MemoryHigh=23552M\nMemoryMax=28672M\nMemorySwapMax=0\n"},
		{"/c/systemd/user/app-Hyprland-com.anthropic.Claude-.scope.d/50-memory-guard.conf", "MemoryHigh=47104M\nMemoryMax=57344M\nMemorySwapMax=4096M\nOOMPolicy=continue\n"},
	} {
		if files[i].Path != want.path || !strings.Contains(string(files[i].Content), want.content) || files[i].Service != (i == 0) {
			t.Errorf("file %d: %s\n%s", i, files[i].Path, files[i].Content)
		}
	}

	files, skipped = systemdSetup{}.Files(SetupSpec{ConfigDir: "/c", Exe: testExe, RAMMiB: 100 << 10})
	if len(files) != 2 || len(skipped) != 1 || !strings.Contains(skipped[0], "no Claude Desktop scope runs") {
		t.Errorf("without a desktop scope: files %d, skipped %v", len(files), skipped)
	}

	files, _ = systemdSetup{}.Files(SetupSpec{ConfigDir: "/c", Exe: testExe, TeleportEvery: 5 * time.Minute})
	if len(files) != 3 {
		t.Fatalf("with the keeper: files %v", files)
	}
	for i, want := range []struct {
		path, content string
		service       bool
	}{
		{"/c/systemd/user/beekeeper-teleport.service", `ExecStart=/b/beekeeper --as "teleport keeper" teleport renew --keeper` + "\n", false},
		{"/c/systemd/user/beekeeper-teleport.timer", "OnUnitActiveSec=300s\n", true},
	} {
		f := files[i+1]
		if f.Path != want.path || !strings.Contains(string(f.Content), want.content) || f.Service != want.service {
			t.Errorf("keeper file %d: %s %v\n%s", i, f.Path, f.Service, f.Content)
		}
	}
}
