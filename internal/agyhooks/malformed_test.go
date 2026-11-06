package agyhooks

import (
	"strings"
	"testing"
)

// agy discards the whole file on a handler or matcher decoding error. A
// healthy beekeeper entry cannot compensate for a malformed sibling.
func TestMalformedFieldsDiscardWholeFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"case 0: unsupported handler type", `{"Stop":[{"type":"http","url":"https://example.invalid/hook"}]}`},
		{"case 1: empty type still needs command", `{"PreInvocation":[{"type":""}]}`},
		{"case 2: string lifecycle timeout", `{"PostInvocation":[{"command":"/usr/bin/true","timeout":"30"}]}`},
		{"case 3: boolean tool timeout", `{"PreToolUse":[{"hooks":[{"command":"/usr/bin/true","timeout":true}]}]}`},
		{"case 4: fractional timeout", `{"Stop":[{"command":"/usr/bin/true","timeout":1.5}]}`},
		{"case 5: timeout over int32", `{"SessionStart":[{"command":"/usr/bin/true","timeout":2147483648}]}`},
		{"case 6: numeric matcher", `{"PreToolUse":[{"matcher":17,"hooks":[{"command":"/usr/bin/true"}]}]}`},
		{"case 7: object matcher", `{"PostToolUse":[{"matcher":{},"hooks":[{"command":"/usr/bin/true"}]}]}`},
		{"case 8: overwritten bad matcher", `{"PreToolUse":[{"matcher":17,"matcher":"*","hooks":[{"command":"/usr/bin/true"}]}]}`},
		{"case 9: overwritten bad timeout", `{"Stop":[{"command":"/usr/bin/true","timeout":"bad","timeout":30}]}`},
		{"case 10: null does not erase unsupported type", `{"Stop":[{"type":"http","type":null,"command":"/usr/bin/true"}]}`},
		{"case 11: bad timeout in replaced event", `{"Stop":[{"command":"/usr/bin/true","timeout":"bad"}],"Stop":[{"command":"/usr/bin/true"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if why := EntryRejects([]byte(tc.raw)); why == "" {
				t.Error("agy rejects this malformed definition")
			}
			why := Rejects([]byte(`{"beekeeper":` + guard + `,"broken":` + tc.raw + `}`))
			if !strings.HasPrefix(why, `the "broken" entry: `) {
				t.Errorf("must identify the entry that discards the guard, got %q", why)
			}
		})
	}
}

func TestReplacedEntryCannotEraseDecodingError(t *testing.T) {
	raw := `{"beekeeper":` + guard + `,"broken":{"Stop":[{"type":"http","command":"/usr/bin/true"}]},"broken":{}}`
	if why := Rejects([]byte(raw)); !strings.HasPrefix(why, `the "broken" entry: `) {
		t.Fatalf("agy discards this file on the earlier decoding error, got %q", why)
	}
}

func TestHandlerFieldDefaultsAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"case 0: omitted type and timeout", `{"Stop":[{"command":"/usr/bin/true"}]}`},
		{"case 1: empty type", `{"PreInvocation":[{"type":"","command":"/usr/bin/true"}]}`},
		{"case 2: null type and timeout", `{"PostInvocation":[{"type":null,"command":"/usr/bin/true","timeout":null}]}`},
		{"case 3: negative timeout", `{"SessionStart":[{"command":"/usr/bin/true","timeout":-1}]}`},
		{"case 4: zero timeout", `{"Stop":[{"command":"/usr/bin/true","timeout":0}]}`},
		{"case 5: largest timeout", `{"Stop":[{"command":"/usr/bin/true","timeout":2147483647}]}`},
		{"case 6: smallest timeout", `{"Stop":[{"command":"/usr/bin/true","timeout":-2147483648}]}`},
		{"case 7: null matcher", `{"PreToolUse":[{"matcher":null,"hooks":[{"command":"/usr/bin/true"}]}]}`},
		{"case 8: replaced missing command", `{"Stop":[{}],"Stop":[{"command":"/usr/bin/true"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if why := EntryRejects([]byte(tc.raw)); why != "" {
				t.Errorf("agy accepts these field values: %s", why)
			}
		})
	}
}
