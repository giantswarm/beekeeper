//go:build linux && !nosystemd

package platform

import (
	"strings"
	"testing"
)

func TestSystemdSetupFiles(t *testing.T) {
	files, skipped := systemdSetup{}.Files(SetupSpec{ConfigDir: "/c", Exe: "/b/beekeeper", RAMMiB: 100 << 10, SwapMiB: 16 << 10,
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

	files, skipped = systemdSetup{}.Files(SetupSpec{ConfigDir: "/c", Exe: "/b/beekeeper", RAMMiB: 100 << 10})
	if len(files) != 2 || len(skipped) != 1 || !strings.Contains(skipped[0], "no Claude Desktop scope runs") {
		t.Errorf("without a desktop scope: files %d, skipped %v", len(files), skipped)
	}
}
