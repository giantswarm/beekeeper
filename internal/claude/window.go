package claude

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
)

// DesktopAppID is the Wayland app id of Claude Desktop's windows.
const DesktopAppID = "com.anthropic.Claude"

// DesktopWindowActive reports whether the compositor's focused window is
// Claude Desktop's: the person is reading or typing in it, and a claude://
// link, which switches its main window to another session, would switch it
// under them. It asks Hyprland (hyprctl activewindow); without a Hyprland
// session there is no focus to ask, and it reports false.
func DesktopWindowActive(ctx context.Context) (bool, error) {
	if os.Getenv("HYPRLAND_INSTANCE_SIGNATURE") == "" {
		return false, nil
	}
	out, err := exec.CommandContext(ctx, "hyprctl", "-j", "activewindow").Output()
	if err != nil {
		return false, err
	}
	return activeIsDesktop(out)
}

// activeIsDesktop reports whether hyprctl's activewindow JSON names the
// desktop's window; hyprctl prints {} when no window has focus.
func activeIsDesktop(out []byte) (bool, error) {
	var w struct {
		Class string `json:"class"`
	}
	if err := json.Unmarshal(out, &w); err != nil {
		return false, err
	}
	return w.Class == DesktopAppID, nil
}
