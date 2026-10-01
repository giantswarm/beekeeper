package guard

import (
	"bytes"
	"encoding/json"
)

// PostToolUseEvent is the hook event a ToolResult names.
const PostToolUseEvent = "PostToolUse"

// ToolResult is the part of a PostToolUse event the hook reads.
type ToolResult struct {
	Event    string          `json:"hook_event_name"`
	Session  string          `json:"session_id"`
	Tool     string          `json:"tool_name"`
	Response json.RawMessage `json:"tool_response"`
}

// PostToolUse scans a tool's result for indexed values and token patterns
// before the model sees it. With a hit it returns the hook's answer, which
// replaces the result with the same result, every hit replaced by a marker
// naming its reference or rule, and the findings; otherwise nil. Malformed
// input, or any other event, gets no answer.
func PostToolUse(input []byte, ix *Index) ([]byte, ToolResult, []Finding) {
	var r ToolResult
	if json.Unmarshal(input, &r) != nil || r.Event != PostToolUseEvent || len(r.Response) == 0 {
		return nil, r, nil
	}
	resp, err := decodeJSON(r.Response)
	if err != nil {
		return nil, r, nil
	}
	var all []Finding
	resp = walkStrings(resp, func(s string) string {
		out, fs := ix.Redact(s)
		all = mergeFindings(all, fs)
		return out
	})
	if len(all) == 0 {
		return nil, r, nil
	}
	type output struct {
		Event  string `json:"hookEventName"`
		Output any    `json:"updatedToolOutput"`
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(struct {
		Out output `json:"hookSpecificOutput"`
	}{output{Event: PostToolUseEvent, Output: resp}}) != nil {
		return nil, r, nil
	}
	return b.Bytes(), r, all
}

// decodeJSON decodes raw keeping numbers as written.
func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}

// walkStrings replaces every string in v, map keys excepted, by fn's answer.
func walkStrings(v any, fn func(string) string) any {
	switch t := v.(type) {
	case string:
		return fn(t)
	case []any:
		for i := range t {
			t[i] = walkStrings(t[i], fn)
		}
	case map[string]any:
		for k := range t {
			t[k] = walkStrings(t[k], fn)
		}
	}
	return v
}

// mergeFindings adds fs to all, summing the counts of the same name.
func mergeFindings(all, fs []Finding) []Finding {
next:
	for _, f := range fs {
		for i := range all {
			if all[i].Ref == f.Ref && all[i].Rule == f.Rule {
				all[i].Count += f.Count
				continue next
			}
		}
		all = append(all, f)
	}
	return all
}
