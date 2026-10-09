//go:build darwin

package platform

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// notifyLabel is the standby service's launchd label.
const notifyLabel = "com.giantswarm.beekeeper.notify"

// nativeSetup is launchd: the standby service as a launch agent of the
// user's GUI session. macOS has no memory guard beekeeper writes.
func nativeSetup() Setup { return launchdSetup{domain: "gui/" + strconv.Itoa(os.Getuid())} }

type launchdSetup struct{ domain string }

func (launchdSetup) Available() bool { return true }

func (launchdSetup) Files(s SetupSpec) ([]File, []string) {
	path := filepath.Join(s.Home, "Library", "LaunchAgents", notifyLabel+".plist")
	args, flag := "watch --standby", ""
	if s.Notify {
		args, flag = "watch --notify --standby", "\n\t\t<string>--notify</string>"
	}
	plist := fmt.Appendf(nil, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- beekeeper %[4]s, the standby watch, written by beekeeper install
     (watch.notify adds --notify). -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%[1]s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%[2]s</string>
		<string>watch</string>%[5]s
		<string>--standby</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>StandardOutPath</key>
	<string>%[3]s</string>
	<key>StandardErrorPath</key>
	<string>%[3]s</string>
</dict>
</plist>
`, notifyLabel, escapeXML(s.Exe), escapeXML(filepath.Join(s.Home, "Library", "Logs", "beekeeper-notify.log")), args, flag)
	skipped := []string{"memory guard: not available on " + Name()}
	if s.TeleportEvery > 0 {
		skipped = append(skipped, "teleport keeper: not available on "+Name())
	}
	return []File{{Path: path, Content: plist, Service: true}}, skipped
}

func escapeXML(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// target is the agent of the plist at path in the user's domain.
func (l launchdSetup) target(path string) string {
	return l.domain + "/" + strings.TrimSuffix(filepath.Base(path), ".plist")
}

func (l launchdSetup) Started(ctx context.Context, path string) bool {
	return exec.CommandContext(ctx, "launchctl", "print", l.target(path)).Run() == nil //nolint:gosec // the user's domain and the label install writes
}

func (launchdSetup) Reload() []string { return nil }

// Start loads the agent from its plist, which RunAtLoad starts.
func (l launchdSetup) Start(path string) []string {
	return []string{"launchctl", "bootstrap", l.domain, path}
}

func (l launchdSetup) Stop(path string) []string {
	return []string{"launchctl", "bootout", l.target(path)}
}
