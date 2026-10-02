package claude

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
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
	buf, err := logTail(logPath)
	if err != nil {
		return "", err
	}
	return lastFocus(buf), nil
}

// logTail is the last focusTail bytes of the desktop's log.
func logTail(logPath string) ([]byte, error) {
	f, err := os.Open(filepath.Clean(logPath))
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read only
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	off := max(fi.Size()-focusTail, 0)
	return io.ReadAll(io.NewSectionReader(f, off, fi.Size()-off))
}

// governorLine is a line of the desktop's CLI governor: "at cap=<n>; would
// evict …" names its cap, "at cap; yielding warm spawn" declines to warm a
// session, since as many CLIs run as the cap allows and it evicts none.
var governorLine = regexp.MustCompile(`(?m)^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) .*\[CliGovernor\] at cap(?:=(\d+);|; (yielding) warm spawn)`)

// DesktopAtCap reports whether the desktop's log holds a warm spawn declined
// at or after since, and the cap its governor last named (0: none named).
func DesktopAtCap(logPath string, since time.Time) (int, bool, error) {
	buf, err := logTail(logPath)
	if err != nil {
		return 0, false, err
	}
	n, ok := atCap(buf, since)
	return n, ok, nil
}

func atCap(log []byte, since time.Time) (int, bool) {
	since = since.Truncate(time.Second)
	var limit, declined int
	var ok bool
	for _, m := range governorLine.FindAllSubmatch(log, -1) {
		if len(m[2]) > 0 {
			limit, _ = strconv.Atoi(string(m[2]))
			continue
		}
		if at, err := time.ParseInLocation(time.DateTime, string(m[1]), time.Local); err == nil && !at.Before(since) {
			declined, ok = limit, true
		}
	}
	return declined, ok
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
