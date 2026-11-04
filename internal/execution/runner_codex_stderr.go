package execution

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
)

const (
	codexStderrLimit = 64 << 10
	codexStderrEvent = "harness.stderr"
)

// Authentication output may contain opaque values no token pattern recognizes,
// or values on the line after their label. Suppress that diagnostic as a whole.
var codexSensitiveStderr = regexp.MustCompile(`(?i)token|authorization|bearer|password|cookie|api[ _-]?key|credential|secret|private[ _-]?key|gitleaks:allow`)

// Opaque values can appear without a label. Prefer losing a long path or
// identifier over retaining a credential in a startup diagnostic.
var codexOpaqueStderr = regexp.MustCompile(`[A-Za-z0-9_+/=.\-]{20,}`)

// codexStderr is written only by os/exec's stderr copier. Read it after Wait,
// so diagnostics cannot race or interleave with the stdout JSON event stream.
type codexStderr struct {
	data      []byte
	truncated bool
}

func (w *codexStderr) Write(p []byte) (int, error) {
	n := min(len(p), codexStderrLimit-len(w.data))
	w.data = append(w.data, p[:n]...)
	w.truncated = w.truncated || n < len(p)
	return len(p), nil
}

func (w *codexStderr) writeLog(log io.Writer) error {
	if len(w.data) == 0 && !w.truncated {
		return nil
	}
	data := w.data
	if w.truncated {
		// Never expose the truncated prefix of a credential or other value.
		data = data[:bytes.LastIndexByte(data, '\n')+1]
	}
	text := "[redacted authentication diagnostic]"
	if !codexSensitiveStderr.Match(w.data) {
		text = codexOpaqueStderr.ReplaceAllString(string(data), "[redacted opaque value]")
	}
	return json.NewEncoder(log).Encode(struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		Truncated bool   `json:"truncated,omitempty"`
	}{Type: codexStderrEvent, Text: text, Truncated: w.truncated})
}
