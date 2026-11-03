package kube

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A detail within messageMax bytes is the message whole; a longer one is cut
// at a character boundary within messageMax, ended by the ellipsis.
func TestClipMessage(t *testing.T) {
	fits := strings.Repeat("a", messageMax)
	if got := clipMessage(fits); got != fits {
		t.Errorf("a detail of %d bytes: clipped to %d", messageMax, len(got))
	}
	for _, detail := range []string{strings.Repeat("a", messageMax+1), strings.Repeat("ä", messageMax), "a" + strings.Repeat("€", messageMax)} {
		got := clipMessage(detail)
		if len(got) > messageMax || !utf8.ValidString(got) || !strings.HasSuffix(got, ellipsis) || !strings.HasPrefix(detail, strings.TrimSuffix(got, ellipsis)) {
			t.Errorf("clipMessage of %d bytes: %d bytes, valid %v: %q", len(detail), len(got), utf8.ValidString(got), got[max(0, len(got)-10):])
		}
	}
}
