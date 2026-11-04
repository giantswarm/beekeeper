package agyhooks

import (
	"testing"
)

// agy reads enabled as metadata and ignores unknown definition fields. A
// valid sibling must not make install or worker admission report a lost guard.
func TestDefinitionMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"case 0: explicitly enabled", `{"enabled":true,"PreInvocation":[{"command":"/usr/bin/true"}]}`},
		{"case 1: explicitly disabled", `{"enabled":false,"PreInvocation":[{"command":"/usr/bin/true"}]}`},
		{"case 2: null enabled", `{"enabled":null,"PreInvocation":[{"command":"/usr/bin/true"}]}`},
		{"case 3: unknown scalar", `{"FutureEvent":17}`},
		{"case 4: unknown event items", `{"FutureEvent":[{"futureHandler":true}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if why := EntryRejects([]byte(tc.raw)); why != "" {
				t.Errorf("agy accepts this definition: %s", why)
			}
			if why := Rejects([]byte(`{"beekeeper":` + guard + `,"other":` + tc.raw + `}`)); why != "" {
				t.Errorf("valid metadata must not discard the sibling guard: %s", why)
			}
		})
	}
}

func TestDefinitionMetadataStillValidatesKnownFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"case 0: string enabled", `{"enabled":"false"}`},
		{"case 1: list enabled", `{"enabled":[]}`},
		{"case 2: disabled malformed handler", `{"enabled":false,"PreInvocation":[{}]}`},
		{"case 3: malformed known event", `{"FutureEvent":17,"Stop":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if why := EntryRejects([]byte(tc.raw)); why == "" {
				t.Fatal("agy rejects this definition, including when disabled")
			}
		})
	}
}
