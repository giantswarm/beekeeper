package guard

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestEncodedAllowMarkerKeepsIndexedValues(t *testing.T) {
	ix := plantedIndex(t)
	encode := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	document := "fixture prefix\nTOKEN=" + planted + "\nfixture suffix\n"
	for name, encoded := range map[string]string{
		"one encoding":  encode(document),
		"two encodings": encode("config: " + encode(document)),
		"decoded mark":  encode(document + " # " + allowMarker),
	} {
		t.Run(name, func(t *testing.T) {
			text := "config: " + encoded + " # " + allowMarker
			out, found := ix.Redact(text)
			if strings.Contains(out, encoded) || len(found) != 1 || found[0].Ref != plantedRef {
				t.Fatal("an allow marker exempted an indexed value in an encoded document")
			}
		})
	}
	// A marker still exempts pattern-only fixtures, even through two encodings.
	pattern := "ghp_" + strings.Repeat("A", 36)
	for _, encoded := range []string{encode(pattern), encode("token: " + encode(pattern))} {
		text := encoded + " # " + allowMarker
		if out, found := ix.Redact(text); out != text || len(found) != 0 {
			t.Fatal("a marked pattern-only fixture was redacted")
		}
	}
}
