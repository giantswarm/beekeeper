package agyhooks

import (
	"encoding/json"
	"fmt"
)

// handlerFields mirrors agy's hookHandlerJSON, including its signed 32-bit
// timeout. Decoding must reject unsupported types before whole-entry command
// validation: even a later duplicate event cannot erase a decoding error.
type handlerFields struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int32  `json:"timeout"`
}

func (h *handlerFields) UnmarshalJSON(raw []byte) error {
	type fields handlerFields
	var decoded fields
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	if decoded.Type != "" && decoded.Type != "command" {
		return fmt.Errorf("unsupported hook type: %q", decoded.Type)
	}
	*h = handlerFields(decoded)
	return nil
}

type matcherFields struct {
	Matcher string          `json:"matcher"`
	Hooks   []handlerFields `json:"hooks"`
}

// fieldTypesRejects checks decoding independently of final-value validation.
// In particular, a replaced command may be missing, but a replaced field
// with the wrong JSON type still makes agy's decoder discard the file.
func fieldTypesRejects(raw []byte) string {
	var fields struct {
		Enabled        bool            `json:"enabled"`
		SessionStart   []handlerFields `json:"SessionStart"`
		PreInvocation  []handlerFields `json:"PreInvocation"`
		PostInvocation []handlerFields `json:"PostInvocation"`
		Stop           []handlerFields `json:"Stop"`
		PreToolUse     []matcherFields `json:"PreToolUse"`
		PostToolUse    []matcherFields `json:"PostToolUse"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "invalid hook fields: " + err.Error()
	}
	return ""
}
