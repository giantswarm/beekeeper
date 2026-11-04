package agyhooks

import "testing"

// Inert entries do not invalidate agy's hooks file. They must not prevent
// the independent beekeeper guard from loading or block worker admission.
func TestInertDefinitionsPreserveSiblingGuard(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"case 0: null definition", nullDefinition},
		{"case 1: no matcher hooks", `{"PreToolUse":[{"matcher":"*"}]}`},
		{"case 2: empty matcher hooks", `{"PostToolUse":[{"matcher":"*","hooks":[]}]}`},
		{"case 3: null matcher hooks", `{"PreToolUse":[{"hooks":null}]}`},
		{"case 4: null matcher", `{"PostToolUse":[null]}`},
		{"case 5: whitespace shell command", `{"Stop":[{"command":"   "}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if why := EntryRejects([]byte(tc.raw)); why != "" {
				t.Errorf("agy accepts this inert definition: %s", why)
			}
			if why := Rejects([]byte(`{"other":` + tc.raw + `,"beekeeper":` + guard + `}`)); why != "" {
				t.Errorf("an inert sibling must not discard the guard: %s", why)
			}
		})
	}
}

func TestInertDefinitionsStillValidateActualHandlers(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"case 0: empty lifecycle handler", `{"Stop":[{}]}`},
		{"case 1: null lifecycle handler", `{"PreInvocation":[null]}`},
		{"case 2: empty nested handler", `{"PreToolUse":[{"hooks":[{}]}]}`},
		{"case 3: null nested handler", `{"PostToolUse":[{"hooks":[null]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if why := EntryRejects([]byte(tc.raw)); why == "" {
				t.Fatal("agy rejects an actual handler without a command")
			}
		})
	}
}
