package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Turn is one message of a transcript: what a person, a peer or the model
// said, without tool calls and tool results.
type Turn struct {
	At   time.Time `json:"at"`
	Role string    `json:"role"`
	Text string    `json:"text"`
}

// tailWindow is how much of a transcript's end is read: tool results make
// lines large, and the last few turns are what is asked for.
const tailWindow = 2 << 20

// Tail returns the last n turns of the transcript at path.
func Tail(path string, n int) ([]Turn, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	off := max(fi.Size()-tailWindow, 0)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	if off > 0 {
		if _, err := r.ReadBytes('\n'); err != nil { // a partial first line
			return nil, err
		}
	}
	var turns []Turn
	for {
		line, err := r.ReadBytes('\n')
		if t, ok := parseTurn(bytes.TrimSpace(line)); ok {
			turns = append(turns, t)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if n > 0 && len(turns) > n {
		turns = turns[len(turns)-n:]
	}
	return turns, nil
}

// firstWindow bounds how much of a transcript's start First reads.
const firstWindow = 4 << 20

// First returns the first turn a person or a starter gave the session: its
// brief. found is false when the transcript's start holds none.
func First(path string) (t Turn, found bool, err error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return Turn{}, false, err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(io.LimitReader(f, firstWindow), 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if t, ok := parseTurn(bytes.TrimSpace(line)); ok && t.Role == roleUser {
			return t, true, nil
		}
		if err == io.EOF {
			return Turn{}, false, nil
		}
		if err != nil {
			return Turn{}, false, err
		}
	}
}

// The roles of a transcript's message entries.
const (
	roleUser      = "user"
	roleAssistant = "assistant"
)

type entry struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	IsMeta    bool      `json:"isMeta"`
	Message   struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func parseTurn(line []byte) (Turn, bool) {
	if len(line) == 0 {
		return Turn{}, false
	}
	var e entry
	if json.Unmarshal(line, &e) != nil || e.IsMeta || (e.Type != roleUser && e.Type != roleAssistant) {
		return Turn{}, false
	}
	var text []string
	var s string
	if json.Unmarshal(e.Message.Content, &s) == nil {
		text = append(text, s)
	} else {
		var blocks []block
		if json.Unmarshal(e.Message.Content, &blocks) != nil {
			return Turn{}, false
		}
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				text = append(text, b.Text)
			}
		}
	}
	joined := strings.TrimSpace(strings.Join(text, "\n"))
	if joined == "" || strings.HasPrefix(joined, "<system-reminder>") {
		return Turn{}, false
	}
	return Turn{At: e.Timestamp, Role: e.Type, Text: joined}, true
}
