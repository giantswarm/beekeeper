package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// desktopEntrypoint is the entrypoint of a transcript entry the desktop's
// CLI wrote: a prompt typed in the desktop's composer, or a message, a
// notice or a notification delivered to the session there.
const desktopEntrypoint = "claude-desktop"

// notTyped are the openings of a desktop prompt no person typed: tags (task
// notifications, system reminders, commands) and peer deliveries.
var notTyped = []string{"<", "Another Claude session sent a message", "[Cross-session delivery notice]"}

// PersonTyped reports whether a person typed a prompt into the session of
// the transcript at path in the desktop: a user entry of the desktop's CLI
// that is no meta entry, no tool result and opens like none of notTyped.
// A prompt it cannot tell apart counts as typed.
func PersonTyped(path string) (bool, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 64<<20)
	for sc.Scan() {
		var e struct {
			entry
			Entrypoint string `json:"entrypoint"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Type != roleUser || e.IsMeta || e.Entrypoint != desktopEntrypoint {
			continue
		}
		if typedText(e.Message.Content) {
			return true, nil
		}
	}
	return false, sc.Err()
}

// typedText reports whether a user entry's content is a prompt a person
// typed.
func typedText(content json.RawMessage) bool {
	var texts []string
	var s string
	if json.Unmarshal(content, &s) == nil {
		texts = append(texts, s)
	} else {
		var blocks []block
		if json.Unmarshal(content, &blocks) != nil {
			return false
		}
		for _, b := range blocks {
			switch b.Type {
			case blockToolResult:
				return false
			case blockText:
				texts = append(texts, b.Text)
			}
		}
	}
	for _, t := range texts {
		t = strings.TrimSpace(t)
		if t != "" && !hasAnyPrefix(t, notTyped) {
			return true
		}
	}
	return false
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
