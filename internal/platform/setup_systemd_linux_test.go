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
		CPUQuota: "1200%", CPUWeight: 50, DesktopScope: "app-Hyprland-com.anthropic.Claude-5776.scope"})
	if len(skipped) > 0 || len(files) != 5 {
		t.Fatalf("files %v, skipped %v", files, skipped)
	}
	for i, want := range []struct{ path, content string }{
		{"/c/systemd/user/beekeeper-notify.service", "ExecStart=/b/beekeeper watch --standby\n"},
		{"/c/systemd/user/beekeeper-sandbox.service", "ExecStart=/b/beekeeper sandbox broker\n"},
		{"/c/systemd/user/beekeeper-supervisor-loop.service", "ExecStart=:\"/b/beekeeper\" supervisor loop\n"},
		{"/c/systemd/user/memcap.slice", "MemoryHigh=23552M\nMemoryMax=28672M\nMemorySwapMax=0\nCPUQuota=1200%\nCPUWeight=50\n"},
		{"/c/systemd/user/app-Hyprland-com.anthropic.Claude-.scope.d/50-memory-guard.conf", "MemoryHigh=47104M\nMemoryMax=57344M\nMemorySwapMax=4096M\nOOMPolicy=continue\n"},
	} {
		if files[i].Path != want.path || !strings.Contains(string(files[i].Content), want.content) || files[i].Service != (i < 3) {
			t.Errorf("file %d: %s\n%s", i, files[i].Path, files[i].Content)
		}
	}

	for _, notify := range []bool{false, true} {
		watch := "watch --standby"
		if notify {
			watch = "watch --notify --standby"
		}
		files, _ = systemdSetup{}.Files(SetupSpec{ConfigDir: "/c", Exe: testExe, Notify: notify})
		unit := string(files[0].Content)
		if !strings.HasPrefix(unit, "# beekeeper "+watch+" as a systemd user unit") ||
			!strings.Contains(unit, "\nExecStart=/b/beekeeper "+watch+"\n") {
			t.Errorf("watch.notify %v: the header and ExecStart do not both run %q:\n%s", notify, watch, unit)
		}
	}

	restart := systemdSetup{}.Restart("/c/systemd/user/beekeeper-notify.service")
	if len(restart) != 1 || strings.Join(restart[0], " ") != "systemctl --user restart beekeeper-notify.service" {
		t.Errorf("restart %v: a linked unit survives only a restart, never a disable", restart)
	}

	files, skipped = systemdSetup{}.Files(SetupSpec{ConfigDir: "/c", Exe: testExe, RAMMiB: 100 << 10})
	if len(files) != 4 || len(skipped) != 1 || !strings.Contains(skipped[0], "no Claude Desktop scope runs") {
		t.Errorf("without a desktop scope: files %d, skipped %v", len(files), skipped)
	}

	files, _ = systemdSetup{}.Files(SetupSpec{ConfigDir: "/c", Exe: testExe, TeleportEvery: 5 * time.Minute})
	if len(files) != 5 {
		t.Fatalf("with the keeper: files %v", files)
	}
	for i, want := range []struct {
		path, content string
		service       bool
	}{
		{"/c/systemd/user/beekeeper-teleport.service", `ExecStart=/b/beekeeper --as "teleport keeper" teleport renew --keeper` + "\n", false},
		{"/c/systemd/user/beekeeper-teleport.timer", "OnCalendar=*:0/5\nPersistent=true\n", true},
	} {
		f := files[i+3]
		if f.Path != want.path || !strings.Contains(string(f.Content), want.content) || f.Service != want.service {
			t.Errorf("keeper file %d: %s %v\n%s", i, f.Path, f.Service, f.Content)
		}
	}
}

// TestCalendar is the keeper timer's calendar event: a wall-clock event
// fires the tick a suspend missed at resume, where a monotonic interval
// waits up to a whole interval after it.
func TestCalendar(t *testing.T) {
	for every, want := range map[time.Duration]string{
		30 * time.Second:  "*:*:0/30",
		0:                 "*:*:0/1",
		10 * time.Minute:  "*:0/10",
		90 * time.Second:  "*:0/1",
		time.Hour:         "0/1:00",
		150 * time.Minute: "0/2:00",
		48 * time.Hour:    "0/23:00",
	} {
		if got := calendar(every); got != want {
			t.Errorf("calendar(%s) = %q, want %q", every, got, want)
		}
	}
}
