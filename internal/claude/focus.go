package claude

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
)

// focusLine is what the desktop app logs each time its main window shows
// another session: sessionId=local_<id>, or null for a view that is none.
var focusLine = []byte("LocalSessions.setFocusedSession: sessionId=")

// focusTail bounds how much of the log DesktopFocus reads: the desktop logs
// a focus change on every switch, so its latest one is near the end.
const focusTail = 1 << 20

// DesktopFocus is the host session id (local_…) the desktop app's main
// window shows, from the last focus change in its log; empty when it shows
// no session or the log holds no focus change.
func DesktopFocus(logPath string) (string, error) {
	f, err := os.Open(filepath.Clean(logPath))
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read only
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	off := max(fi.Size()-focusTail, 0)
	buf, err := io.ReadAll(io.NewSectionReader(f, off, fi.Size()-off))
	if err != nil {
		return "", err
	}
	return lastFocus(buf), nil
}

func lastFocus(log []byte) string {
	i := bytes.LastIndex(log, focusLine)
	if i < 0 {
		return ""
	}
	id, _, _ := bytes.Cut(log[i+len(focusLine):], []byte("\n"))
	id = bytes.TrimSpace(id)
	if !bytes.HasPrefix(id, []byte("local_")) {
		return ""
	}
	return string(id)
}
