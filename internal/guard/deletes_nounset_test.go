package guard

import "testing"

func TestDeleteGuardKnownEmptyPrefixWithNounset(t *testing.T) {
	for _, command := range []string{
		`set -u; D=; rm -rf "$D"/var/lib/fixture`,
		`D=''; set -o nounset; shred -u "$D"/var/lib/fixture`,
		`set -eu; export D=""; unlink "$D"/var/lib/fixture`,
		`set -u; D=/tmp/fixture; D=; truncate -s 0 "$D"/var/lib/fixture`,
	} {
		t.Run(command, func(t *testing.T) {
			// Inspect the command only; no deletion is ever executed.
			if deleteRefusal(command) == "" {
				t.Fatal("nounset exempted an explicitly empty delete prefix")
			}
		})
	}
	for _, command := range []string{
		`set -u; D=/tmp/fixture; rm -f "$D"/file`,
		`set -u; D=; rm -f "${D:?}"/file`,
	} {
		if reason := deleteRefusal(command); reason != "" {
			t.Fatalf("safe cleanup refused: %s", reason)
		}
	}
}
